// Command plugin-loader-service hosts the Nexus plugin loader.
//
// It is an independent microservice (deployed alongside encrypt-service,
// decrypt-service, keygen-service, etc.) responsible for:
//
//   - Loading WASM plugins (wazero runtime)
//   - Brokering access to host capabilities (storage, vector, state:kv, ...)
//   - Replicating persistent state through its own Raft cluster
//     (independent from the main cipherlake Raft)
//   - Exposing admin + invocation gRPC APIs to the gateway
//
// Phase 5 (spec §3.7 A4, P5-5, F5-1):
//   - Constructs real IAM/KMS/Audit services and injects them into the
//     Loader's host-imports layer so Tier-2-only capabilities
//     (iam.policy.evaluate, kms.datakey.generate, crypto.audit.sign,
//     gateway.hook.register-for-other) are backed by production services.
//
// See .trae/specs/plugin-system/spec.txt for the full design.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"cipherlake/internal/config"
	"cipherlake/internal/iam"
	"cipherlake/internal/kms"
	"cipherlake/internal/pluginloader"
	"cipherlake/internal/scheduler"
	"cipherlake/internal/vector"
	pluginpb "cipherlake/proto/plugin"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	flag.Parse()

	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	zap.ReplaceGlobals(logger)
	defer logger.Sync()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Fatal("failed to load config", zap.Error(err), zap.String("path", *configPath))
	}

	if cfg.PluginLoader.Raft == nil || !cfg.PluginLoader.Raft.Enabled {
		logger.Fatal("plugin loader disabled: plugin_loader.raft.enabled is false")
	}
	if cfg.PluginLoader.Raft.DataDir == "" {
		logger.Fatal("plugin_loader.raft.data_dir is required")
	}
	if cfg.PluginLoader.GRPCListenAddr == "" {
		logger.Fatal("plugin_loader.grpc_listen_addr is required")
	}

	loader, err := pluginloader.New(&cfg.PluginLoader)
	if err != nil {
		logger.Fatal("failed to create plugin loader", zap.Error(err))
	}
	defer loader.Shutdown()

	// Wire Tier-2-only service implementations (spec §3.7 A4, P5-5, F5-1).
	// These back the iam.policy.evaluate / kms.datakey.generate /
	// crypto.audit.sign / gateway.hook.register-for-other host imports.
	if err := wireTier2Services(loader, cfg, logger); err != nil {
		logger.Warn("tier-2 service wiring incomplete; Tier-2-only host imports will return ErrInternal", zap.Error(err))
	}

	// Wire the system scheduler so plugins can register cron tasks (P4-3).
	sched := scheduler.NewScheduler()
	loader.SetScheduler(sched)
	if err := sched.Start(); err != nil {
		logger.Fatal("failed to start scheduler", zap.Error(err))
	}
	defer sched.Stop()

	// Start the gRPC server on GRPCListenAddr.
	grpcLn, err := net.Listen("tcp", cfg.PluginLoader.GRPCListenAddr)
	if err != nil {
		logger.Fatal("failed to listen for grpc", zap.String("addr", cfg.PluginLoader.GRPCListenAddr), zap.Error(err))
	}
	defer grpcLn.Close()

	grpcServer := grpc.NewServer()
	pluginpb.RegisterPluginLoaderServiceServer(grpcServer, &pluginloader.LoaderGRPCServer{Loader: loader})
	pluginpb.RegisterPluginAdminServiceServer(grpcServer, &pluginloader.LoaderAdminGRPCServer{Loader: loader})

	go func() {
		logger.Info("plugin-loader-service grpc listening", zap.String("addr", cfg.PluginLoader.GRPCListenAddr))
		if err := grpcServer.Serve(grpcLn); err != nil {
			logger.Error("grpc server error", zap.Error(err))
		}
	}()

	// Optional HTTP admin server for healthz/readyz/metrics.
	var httpSrv *http.Server
	if cfg.PluginLoader.AdminListenAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			// Ready once we can read from the FSM db. A more sophisticated
			// check (leader confirmed, plugin_registry loaded) lands in P3-5.
			if _, err := loader.StateGet("__probe__", "__probe__"); err == nil {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ready"))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
		})
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			// Placeholder: real Prometheus exporter added in P4-4.
			w.WriteHeader(http.StatusOK)
		})

		// HTTP entry point for invoking plugin routes directly.
		// Path format: /invoke/{plugin_name}/{rest...}
		// Example:   POST /invoke/mcp-memory-server/mcp/messages
		// This lets the loader be usable standalone (without the gateway)
		// for development, debugging, and tool integration (e.g. MCP
		// clients pointing directly at the loader).
		mux.HandleFunc("/invoke/", func(w http.ResponseWriter, r *http.Request) {
			handleInvokeRoute(w, r, loader, logger)
		})

		// Convenience: list installed plugins.
		mux.HandleFunc("/plugins", func(w http.ResponseWriter, r *http.Request) {
			handleListPlugins(w, r, loader)
		})

		// HTTP entry point for admin operations: install / uninstall / reload.
		// Path format:
		//   POST /admin/plugin/install   (multipart: manifest + wasm + optional sig + key_id)
		//   POST /admin/plugin/uninstall (form: plugin_name=...)
		//   POST /admin/plugin/reload     (form: plugin_name=...)
		mux.HandleFunc("/admin/plugin/", func(w http.ResponseWriter, r *http.Request) {
			handleAdminPlugin(w, r, loader, logger)
		})

		httpSrv = &http.Server{
			Addr:              cfg.PluginLoader.AdminListenAddr,
			Handler:           mux,
			ReadHeaderTimeout: 30 * time.Second,
		}
		go func() {
			logger.Info("plugin-loader-service admin http listening", zap.String("addr", cfg.PluginLoader.AdminListenAddr))
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("admin http server error", zap.Error(err))
			}
		}()
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("shutting down on signal", zap.String("signal", sig.String()))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	grpcServer.GracefulStop()
	if httpSrv != nil {
		if err := httpSrv.Shutdown(ctx); err != nil {
			logger.Error("admin http server shutdown error", zap.Error(err))
		}
	}
}

// loadConfig reads the YAML/JSON config file from path and returns a
// populated *config.Config. It reuses the cipherlake/internal/config
// loader so the plugin-loader shares the same config schema as the main
// cipherlake binary.
func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		return nil, fmt.Errorf("config path is empty")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	return cfg, nil
}

// wireTier2Services constructs real IAM, KMS, and audit-signing service
// implementations and injects them into the Loader's host-imports layer.
// Any non-fatal initialization error is logged and returned so the caller
// can decide whether to proceed without the corresponding Tier-2-only host
// import (spec §3.7 A4, P5-5, F5-1).
func wireTier2Services(loader *pluginloader.Loader, cfg *config.Config, logger *zap.Logger) error {
	// 1. IAM policy evaluator (iam.policy.evaluate).
	// Construct an IAMStore from the configured IAM database path; if no
	// store is available, the evaluator returns ImplicitDeny for all
	// requests, which is the safe default.
	if cfg.IAM.Enabled && cfg.IAM.DBPath != "" {
		store, err := iam.NewIAMStore(cfg.IAM.DBPath)
		if err != nil {
			logger.Warn("tier-2: failed to open IAM store", zap.Error(err))
		} else {
			loader.SetPolicyEvaluator(iam.NewPolicyEvaluator(store))
			logger.Info("tier-2: IAM policy evaluator wired", zap.String("db_path", cfg.IAM.DBPath))
		}
	}

	// 2. KMS data key generator (kms.datakey.generate).
	// Use LocalKMS backed by the configured crypto-services key path. This
	// generates and encrypts data encryption keys using ECIES-P256.
	kmsKeyPath := "./data/keys/keygen"
	if cfg.CryptoServices.KeyPath != "" {
		kmsKeyPath = cfg.CryptoServices.KeyPath
	}
	localKMS, err := kms.NewLocalKMS(kms.LocalConfig{KeyPath: kmsKeyPath})
	if err != nil {
		logger.Warn("tier-2: failed to initialize local KMS", zap.Error(err))
	} else {
		loader.SetDataKeyGenerator(localKMS)
		logger.Info("tier-2: KMS data key generator wired", zap.String("key_path", kmsKeyPath))
	}

	// 3. Audit signer (crypto.audit.sign).
	// ECDSAAuditSigner loads or generates a P-256 key pair for signing
	// audit payloads.
	auditKeyPath := filepath.Join(filepath.Dir(kmsKeyPath), "audit")
	signer, err := pluginloader.NewECDSAAuditSigner(auditKeyPath, "plugin-loader-audit")
	if err != nil {
		logger.Warn("tier-2: failed to initialize audit signer", zap.Error(err))
	} else {
		loader.SetAuditSigner(signer)
		logger.Info("tier-2: audit signer wired", zap.String("key_path", auditKeyPath))
	}

	// 4. Hook registrar (gateway.hook.register-for-other).
	// The Loader itself implements HookRegistrar; this is already wired
	// by default in pluginloader.New (see loader.go). No action needed.

	// 5. Vector Search Engine (vector.search).
	// Wire in the cipherlake VSE (VectorManager) so plugins can run real
	// vector searches against the same Milvus backend used by the
	// cipherlake gateway. When Milvus is not configured, the VSE still
	// initializes but Search returns ErrIndexNotInitialized; plugins
	// see statusInternal in that case.
	if cfg.Vector.Enabled {
		vcfg := buildVectorConfig(&cfg.Vector)
		vm, err := vector.NewVectorManager(vcfg)
		if err != nil {
			logger.Warn("tier-2: failed to initialize vector manager", zap.Error(err))
		} else {
			loader.SetVectorSearcher(vm)
			logger.Info("tier-2: vector searcher wired",
				zap.Int("dim", vcfg.Dimension),
				zap.String("metric", vcfg.MetricType),
				zap.Bool("milvus", vcfg.Milvus != nil))
		}
	} else {
		logger.Info("tier-2: vector disabled in config; vector.search will return ErrInternal")
	}

	// 6. Storage client (storage.get / storage.put).
	// Wire in a gateway-backed storage client that walks the cipherlake
	// standard read/write paths over HTTP:
	//   GET: auth → metadata → backend.Get → DecryptOperation
	//   PUT: auth → EncryptOperation → backend.Put → metadata index
	// Plugins receive decrypted plaintext bytes on read and present
	// plaintext bytes on write; they never touch raw encrypted blobs
	// on disk. storage.list/delete remain Unauthorized - the host
	// import ABI only exposes get/put.
	if cfg.PluginLoader.GatewayStorageEndpoint != "" {
		sc := newGatewayStorageClient(cfg.PluginLoader.GatewayStorageEndpoint, cfg.PluginLoader.GatewayStorageServiceToken, logger)
		loader.SetStorageClient(sc)
		logger.Info("tier-2: storage client wired",
			zap.String("gateway_endpoint", cfg.PluginLoader.GatewayStorageEndpoint))
	} else {
		logger.Info("tier-2: gateway_storage_endpoint not set; storage.get/put will return Unauthorized")
	}

	return nil
}

// buildVectorConfig translates the cipherlake vector config block into
// the VectorConfig expected by vector.NewVectorManager. The VSE is a
// standalone module; it does not require the gateway to be running in
// the same process.
func buildVectorConfig(cfg *config.VectorConfig) *vector.VectorConfig {
	vcfg := &vector.VectorConfig{
		Enabled:              cfg.Enabled,
		Dimension:            cfg.Dimension,
		IndexType:            cfg.IndexType,
		MetricType:           cfg.MetricType,
		MaxVectors:           cfg.MaxVectors,
		EmbeddingProvider:    cfg.EmbeddingProvider,
		EmbeddingModelPath:   cfg.EmbeddingModelPath,
		EmbeddingAPIEndpoint: cfg.EmbeddingAPIEndpoint,
		EmbeddingAPIKey:      cfg.EmbeddingAPIKey,
		EmbeddingModelName:   cfg.EmbeddingModelName,
		AutoIndex:            cfg.AutoIndex,
		MaxSearchTopK:        cfg.MaxSearchTopK,
		MaxQueryLength:       cfg.MaxQueryLength,
		RequireAuth:          cfg.RequireAuth,
		AllowedContentTypes:  cfg.AllowedContentTypes,
		MaxIndexContentSize:  cfg.MaxIndexContentSize,
		// QueryCacheSize/TTL left at zero so NewVectorManager applies
		// its sensible defaults (10000 entries / 5 min TTL).
	}
	if cfg.Milvus != nil && cfg.Milvus.Address != "" {
		vcfg.Milvus = &vector.MilvusConfig{
			Address:          cfg.Milvus.Address,
			Username:         cfg.Milvus.Username,
			Password:         cfg.Milvus.Password,
			DBName:            cfg.Milvus.DBName,
			CollectionName:   cfg.Milvus.CollectionName,
			ShardsNum:        cfg.Milvus.ShardsNum,
			IndexType:        cfg.Milvus.IndexType,
			IndexParams:      cfg.Milvus.IndexParams,
			NPROBE:           cfg.Milvus.NPROBE,
			EF:               cfg.Milvus.EF,
			ConsistencyLevel: cfg.Milvus.ConsistencyLevel,
			ReplicaNumber:    cfg.Milvus.ReplicaNumber,
		}
	}
	return vcfg
}

// gatewayStorageClient implements pluginloader.StorageClient by issuing
// GET/PUT requests to the cipherlake gateway. The gateway walks the full
// standard read/write path:
//
//	GET: auth → metadata → backend.Get → DecryptOperation (returns plaintext)
//	PUT: auth → EncryptOperation → backend.Put → metadata index (accepts plaintext)
//
// Plugins never touch raw encrypted blobs on disk; the gateway enforces
// bucket policies, KMS-based encryption, IAM audit, and metadata
// indexing on both directions.
//
// The client authenticates with a static service token configured on the
// loader side; the gateway must accept this token as a trusted internal
// caller.
type gatewayStorageClient struct {
	endpoint    string
	serviceTok  string
	httpClient  *http.Client
	logger      *zap.Logger
}

func newGatewayStorageClient(endpoint, serviceToken string, logger *zap.Logger) *gatewayStorageClient {
	return &gatewayStorageClient{
		endpoint:   strings.TrimRight(endpoint, "/"),
		serviceTok: serviceToken,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
	}
}

func (c *gatewayStorageClient) GetObject(ctx context.Context, bucket, key, originalUser string) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/%s", c.endpoint, bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.serviceTok != "" {
		req.Header.Set("Authorization", "Bearer "+c.serviceTok)
	}
	// Pass the originating plugin name as the audit principal. The
	// gateway can use this for audit attribution.
	if originalUser != "" {
		req.Header.Set("X-Plugin-Principal", originalUser)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, pluginloader.ErrNotFound
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("gateway GET %s/%s: status %d: %s", bucket, key, resp.StatusCode, string(body))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20)) // 64MB cap
}

// PutObject forwards a plaintext body to the cipherlake gateway standard
// PUT path. The gateway performs auth, KMS-based EncryptOperation,
// backend.Put, and metadata indexing; the plugin never handles
// ciphertext or encryption keys. Returns the ETag the gateway assigns to
// the stored object.
func (c *gatewayStorageClient) PutObject(ctx context.Context, bucket, key string, body []byte, originalUser string) (string, error) {
	url := fmt.Sprintf("%s/%s/%s", c.endpoint, bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	if c.serviceTok != "" {
		req.Header.Set("Authorization", "Bearer "+c.serviceTok)
	}
	if originalUser != "" {
		req.Header.Set("X-Plugin-Principal", originalUser)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(body))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("gateway PUT %s/%s: status %d: %s", bucket, key, resp.StatusCode, string(respBody))
	}
	// The gateway returns the ETag in the response header (standard S3
	// semantics). Strip the surrounding quotes if present.
	etag := resp.Header.Get("ETag")
	if etag != "" {
		etag = strings.Trim(etag, `"`)
	}
	return etag, nil
}

// handleInvokeRoute dispatches an HTTP request to a plugin's on_request
// entry point via Loader.InvokeRoute. Path format:
//
//	/invoke/{plugin_name}/{rest...}
//
// Examples:
//
//	POST /invoke/mcp-memory-server/mcp/messages
//	GET  /invoke/mcp-memory-server/mcp/sse
//	POST /invoke/mcp-memory-server/memory/search
//
// The plugin_name segment must match an installed plugin; {rest} is the
// path under the plugin prefix (no leading slash).
func handleInvokeRoute(w http.ResponseWriter, r *http.Request, loader *pluginloader.Loader, logger *zap.Logger) {
	// Strip the "/invoke/" prefix.
	raw := strings.TrimPrefix(r.URL.Path, "/invoke/")
	if raw == "" {
		http.Error(w, "missing plugin name in path", http.StatusBadRequest)
		return
	}
	// Split into plugin_name and rest-of-path on the first '/'.
	pluginName, rest, found := strings.Cut(raw, "/")
	if !found || pluginName == "" {
		http.Error(w, "missing plugin name or path", http.StatusBadRequest)
		return
	}
	routePath := "/" + rest

	// Read request body (capped at 16MB to protect memory).
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, "failed to read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Collect request headers (lowercased keys, single value).
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	resp, err := loader.InvokeRoute(r.Context(), &pluginpb.InvokeRouteRequest{
		PluginName: pluginName,
		Method:     r.Method,
		Path:       routePath,
		Headers:    headers,
		Body:       body,
	})
	if err != nil {
		logger.Warn("invoke-route failed", zap.Error(err), zap.String("plugin", pluginName), zap.String("path", routePath))
		http.Error(w, "plugin invocation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Write plugin response headers first, then status and body.
	for k, v := range resp.GetHeaders() {
		w.Header().Set(k, v)
	}
	status := int(resp.GetStatusCode())
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if _, err := w.Write(resp.GetBody()); err != nil {
		logger.Debug("failed to write response body", zap.Error(err))
	}
}

// handleListPlugins writes a JSON array of installed plugin names to w.
func handleListPlugins(w http.ResponseWriter, r *http.Request, loader *pluginloader.Loader) {
	records, err := loader.ListPlugins(r.Context())
	if err != nil {
		http.Error(w, "list plugins: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type pluginEntry struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Tier    int32  `json:"trust_tier"`
	}
	out := make([]pluginEntry, 0, len(records))
	for _, rec := range records {
		out = append(out, pluginEntry{
			Name:    rec.Name,
			Version: rec.Version,
			Tier:    int32(rec.TrustTier),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleAdminPlugin dispatches admin operations (install / uninstall / reload)
// received over HTTP. The handler accepts multipart/form-data on install
// (fields: manifest, wasm, optional sig, optional key_id) and
// application/x-www-form-urlencoded on uninstall/reload (field: plugin_name).
//
// This is intended for development / manual operation; the gRPC admin API
// remains the production-grade path used by cipherlakectl.
func handleAdminPlugin(w http.ResponseWriter, r *http.Request, loader *pluginloader.Loader, logger *zap.Logger) {
	action := strings.TrimPrefix(r.URL.Path, "/admin/plugin/")
	switch action {
	case "install":
		handleAdminInstall(w, r, loader, logger)
	case "uninstall":
		handleAdminUninstall(w, r, loader, logger)
	case "reload":
		handleAdminReload(w, r, loader, logger)
	case "list":
		handleListPlugins(w, r, loader)
	default:
		http.NotFound(w, r)
	}
}

func handleAdminInstall(w http.ResponseWriter, r *http.Request, loader *pluginloader.Loader, logger *zap.Logger) {
	// Accept either multipart/form-data or JSON body. Multipart is easier
	// to use from curl when uploading binary wasm.
	maxBytes := int64(64 << 20) // 64MB cap (wasm can be large in release builds)
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	var manifestBytes, wasmBytes, sig []byte
	var keyID string

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/"):
		if err := r.ParseMultipartForm(maxBytes); err != nil {
			http.Error(w, "invalid multipart form: "+err.Error(), http.StatusBadRequest)
			return
		}
		if f, _, err := r.FormFile("manifest"); err == nil {
			manifestBytes, _ = io.ReadAll(f)
			f.Close()
		}
		if f, _, err := r.FormFile("wasm"); err == nil {
			wasmBytes, _ = io.ReadAll(f)
			f.Close()
		}
		if f, _, err := r.FormFile("sig"); err == nil {
			sig, _ = io.ReadAll(f)
			f.Close()
		}
		keyID = r.FormValue("key_id")
	case strings.HasPrefix(ct, "application/json"):
		var body struct {
			Manifest   json.RawMessage `json:"manifest"`
			WASM        []byte          `json:"wasm"`
			Signature   []byte          `json:"signature"`
			KeyID       string          `json:"key_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		manifestBytes = []byte(body.Manifest)
		wasmBytes = body.WASM
		sig = body.Signature
		keyID = body.KeyID
	default:
		http.Error(w, "unsupported content type: "+ct, http.StatusUnsupportedMediaType)
		return
	}

	if len(manifestBytes) == 0 || len(wasmBytes) == 0 {
		http.Error(w, "manifest and wasm are required", http.StatusBadRequest)
		return
	}

	manifest, err := pluginloader.ParseManifest(manifestBytes)
	if err != nil {
		http.Error(w, "parse manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := loader.InstallPlugin(r.Context(), manifest, wasmBytes, sig, keyID); err != nil {
		logger.Warn("install failed", zap.Error(err))
		http.Error(w, "install failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"installed":    true,
		"plugin_name":  manifest.Name,
		"version":      manifest.Version,
		"trust_tier":   manifest.TrustTierRequested,
	})
}

func handleAdminUninstall(w http.ResponseWriter, r *http.Request, loader *pluginloader.Loader, logger *zap.Logger) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := r.FormValue("plugin_name")
	if name == "" {
		http.Error(w, "plugin_name is required", http.StatusBadRequest)
		return
	}
	force := r.FormValue("force") == "true" || r.FormValue("force") == "1"
	if err := loader.UninstallPlugin(r.Context(), name, force); err != nil {
		logger.Warn("uninstall failed", zap.Error(err), zap.String("plugin", name))
		http.Error(w, "uninstall failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"uninstalled": true,
		"plugin_name": name,
	})
}

func handleAdminReload(w http.ResponseWriter, r *http.Request, loader *pluginloader.Loader, logger *zap.Logger) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := r.FormValue("plugin_name")
	if name == "" {
		http.Error(w, "plugin_name is required", http.StatusBadRequest)
		return
	}
	if err := loader.ReloadPlugin(r.Context(), name); err != nil {
		logger.Warn("reload failed", zap.Error(err), zap.String("plugin", name))
		http.Error(w, "reload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"reloaded":    true,
		"plugin_name": name,
	})
}
