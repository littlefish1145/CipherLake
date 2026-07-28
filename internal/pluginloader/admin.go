package pluginloader

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cipherlake/internal/config"
	"cipherlake/internal/logger"
	pluginpb "cipherlake/proto/plugin"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	bolt "go.etcd.io/bbolt"
)

// pluginCache holds the on-disk artifact cache for a single plugin.
// The cache is written on every successful install so reload/recovery can
// reconstruct the plugin without re-pulling from OCI (spec §3.11, P3-1).
type pluginCache struct {
	dir string
}

func newPluginCache(baseDir, pluginName string) *pluginCache {
	return &pluginCache{dir: filepath.Join(baseDir, pluginName)}
}

func (c *pluginCache) ensureDir() error {
	if c.dir == "" {
		return errors.New("plugin cache dir is empty")
	}
	return os.MkdirAll(c.dir, 0o755)
}

func (c *pluginCache) WriteManifest(manifest []byte) error {
	if err := c.ensureDir(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "manifest.json"), manifest, 0o644)
}

func (c *pluginCache) WriteWASM(wasm []byte) error {
	if err := c.ensureDir(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "plugin.wasm"), wasm, 0o644)
}

func (c *pluginCache) WriteSignature(sig []byte) error {
	if len(sig) == 0 {
		return nil
	}
	if err := c.ensureDir(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "signature.sig"), sig, 0o644)
}

func (c *pluginCache) WriteKeyID(keyID string) error {
	if keyID == "" {
		return nil
	}
	if err := c.ensureDir(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "key_id.txt"), []byte(keyID), 0o644)
}

func (c *pluginCache) ReadManifest() ([]byte, error) {
	return os.ReadFile(filepath.Join(c.dir, "manifest.json"))
}

func (c *pluginCache) ReadWASM() ([]byte, error) {
	return os.ReadFile(filepath.Join(c.dir, "plugin.wasm"))
}

func (c *pluginCache) ReadSignature() ([]byte, error) {
	path := filepath.Join(c.dir, "signature.sig")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (c *pluginCache) ReadKeyID() (string, error) {
	path := filepath.Join(c.dir, "key_id.txt")
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// cacheDir returns the effective plugin cache directory.
func (l *Loader) cacheDir() string {
	if l.cfg.PluginCacheDir != "" {
		return l.cfg.PluginCacheDir
	}
	return "data/plugin-cache"
}

// writePluginCache persists install artifacts so reload can use them.
func (l *Loader) writePluginCache(record *PluginRecord, manifest, wasm []byte) error {
	cache := newPluginCache(l.cacheDir(), record.Name)
	if err := cache.WriteManifest(manifest); err != nil {
		return fmt.Errorf("cache manifest: %w", err)
	}
	if err := cache.WriteWASM(wasm); err != nil {
		return fmt.Errorf("cache wasm: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(record.SignatureB64)
	if err == nil && len(sig) > 0 {
		if err := cache.WriteSignature(sig); err != nil {
			return fmt.Errorf("cache signature: %w", err)
		}
	}
	if record.KeyID != "" {
		if err := cache.WriteKeyID(record.KeyID); err != nil {
			return fmt.Errorf("cache key_id: %w", err)
		}
	}
	return nil
}

// readPluginCache loads artifacts from the local cache.
func (l *Loader) readPluginCache(pluginName string) (manifest, wasm, sig []byte, keyID string, err error) {
	cache := newPluginCache(l.cacheDir(), pluginName)
	manifest, err = cache.ReadManifest()
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("read cached manifest: %w", err)
	}
	wasm, err = cache.ReadWASM()
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("read cached wasm: %w", err)
	}
	sig, _ = cache.ReadSignature() // optional
	keyID, _ = cache.ReadKeyID()   // optional
	return manifest, wasm, sig, keyID, nil
}

// UninstallPlugin removes a plugin from runtime and persistent state.
// If force is false and other installed plugins declare depends_on this
// plugin, ErrHasDependents is returned. The plugin's on_uninstall entry
// point is invoked before the instance pool is destroyed (spec §3.5).
func (l *Loader) UninstallPlugin(ctx context.Context, name string, force bool) error {
	l.mu.RLock()
	holder, installed := l.plugins[name]
	l.mu.RUnlock()

	dependents, err := l.dependentsOf(name)
	if err != nil {
		return fmt.Errorf("failed to check dependents: %w", err)
	}
	if !force && len(dependents) > 0 {
		return ErrHasDependents{Plugin: name, Dependents: dependents}
	}

	// Call on_uninstall before tearing down the pool. Errors are logged
	// but do not block uninstall.
	if installed && holder != nil && holder.pool != nil {
		if inst, err := holder.pool.Borrow(ctx); err == nil {
			_, _ = inst.CallEntry(ctx, "on_uninstall")
			holder.pool.Return(inst)
		}
	}

	// Close all SSE sessions owned by this plugin before the pool is
	// torn down so on_sse_close can still borrow an instance.
	l.CloseAllSSESessions(ctx, name, SSECloseReasonUninstall)

	// Persist uninstall through Raft before mutating in-memory state.
	op := &LoaderFSMOp{
		Type:   OpPluginUninstall,
		Plugin: name,
	}
	if err := l.RaftApply(ctx, op); err != nil {
		return fmt.Errorf("failed to persist plugin uninstall: %w", err)
	}

	if installed {
		l.unloadPlugin(name)
	}
	return nil
}

// dependentsOf returns the names of installed plugins that declare
// depends_on on the given plugin.
func (l *Loader) dependentsOf(name string) ([]string, error) {
	records, err := l.listPluginRecords()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range records {
		if r.Name == name {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(r.Manifest, &m); err != nil {
			continue
		}
		for _, dep := range m.DependsOn {
			if dep == name {
				out = append(out, r.Name)
				break
			}
		}
	}
	return out, nil
}

// ReloadPlugin uninstalls and reinstalls a plugin from the local artifact
// cache. It is used to pick up a changed trusted_keys tier or to recover
// after a partial install. The plugin is marked reloading while the old
// instance is suspended and the new instance is resumed, so callers receive
// ErrDependencyUnavailable during the window (spec §3.5).
func (l *Loader) ReloadPlugin(ctx context.Context, name string) error {
	manifestBytes, wasmBytes, sig, keyID, err := l.readPluginCache(name)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "plugin %q not found in local cache: %v", name, err)
	}
	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return status.Errorf(codes.Internal, "cached manifest for %q is invalid: %v", name, err)
	}

	// Mark the plugin as reloading so concurrent callers fail-fast with
	// ErrDependencyUnavailable instead of racing the pool teardown.
	l.mu.Lock()
	holder, ok := l.plugins[name]
	if ok {
		holder.reloading = true
	}
	l.mu.Unlock()

	// Call on_suspend on a borrowed instance before tearing it down.
	if ok && holder != nil && holder.pool != nil {
		if inst, err := holder.pool.Borrow(ctx); err == nil {
			_, _ = inst.CallEntry(ctx, "on_suspend")
			holder.pool.Return(inst)
		}
	}

	// Uninstall if currently loaded. Force=true because we are about to
	// reinstall the same plugin.
	if err := l.UninstallPlugin(ctx, name, true); err != nil {
		return fmt.Errorf("reload uninstall failed: %w", err)
	}

	if err := l.InstallPlugin(ctx, manifest, wasmBytes, sig, keyID); err != nil {
		return fmt.Errorf("reload install failed: %w", err)
	}

	// Call on_resume on the freshly-created pool.
	l.mu.RLock()
	newHolder, ok := l.plugins[name]
	l.mu.RUnlock()
	if ok && newHolder != nil && newHolder.pool != nil {
		if inst, err := newHolder.pool.Borrow(ctx); err == nil {
			_, _ = inst.CallEntry(ctx, "on_resume")
			newHolder.pool.Return(inst)
		}
	}

	// Clear the reloading flag.
	l.mu.Lock()
	if newHolder, ok := l.plugins[name]; ok {
		newHolder.reloading = false
	}
	l.mu.Unlock()
	return nil
}

// ListPlugins returns the installed plugin records from the replicated FSM.
func (l *Loader) ListPlugins(ctx context.Context) ([]PluginRecord, error) {
	return l.listPluginRecords()
}

func (l *Loader) listPluginRecords() ([]PluginRecord, error) {
	db := l.fsm.DB()
	if db == nil {
		return nil, errors.New("loader FSM db is nil")
	}
	var records []PluginRecord
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPluginRegistry))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var r PluginRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("corrupt plugin record %s: %w", k, err)
			}
			records = append(records, r)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// GetPluginRecord reads a single plugin record from the FSM registry.
func (l *Loader) GetPluginRecord(name string) (*PluginRecord, error) {
	db := l.fsm.DB()
	if db == nil {
		return nil, errors.New("loader FSM db is nil")
	}
	var record *PluginRecord
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPluginRegistry))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(name))
		if raw == nil {
			return nil
		}
		var r PluginRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("corrupt plugin record %s: %w", name, err)
		}
		record = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// ErrHasDependents is returned when uninstall is blocked by dependent plugins.
type ErrHasDependents struct {
	Plugin     string
	Dependents []string
}

func (e ErrHasDependents) Error() string {
	return fmt.Sprintf("plugin %q has dependents: %v", e.Plugin, e.Dependents)
}

// IsErrHasDependents reports whether err is an ErrHasDependents.
func IsErrHasDependents(err error) bool {
	var e ErrHasDependents
	return errors.As(err, &e)
}

// ociAuthForRegistry returns the auth credentials for the given registry
// host, reading tokens/passwords from environment variables per spec §3.16.
func (l *Loader) ociAuthForRegistry(registryHost string) *OCIAuth {
	for _, rc := range l.cfg.OCRegistries {
		u := rc.URL
		u = strings.TrimPrefix(u, "http://")
		u = strings.TrimPrefix(u, "https://")
		if u != registryHost {
			continue
		}
		switch rc.AuthType {
		case "basic":
			return &OCIAuth{
				Type:     "basic",
				Username: os.Getenv(rc.Auth.UsernameEnv),
				Password: os.Getenv(rc.Auth.PasswordEnv),
			}
		case "bearer":
			return &OCIAuth{Type: "bearer", Token: os.Getenv(rc.Auth.TokenEnv)}
		}
	}
	return nil
}

// pullOCI fetches a plugin artifact from an OCI URL and returns its
// manifest, wasm, signature and key ID. The artifact is also written to
// the local plugin cache.
func (l *Loader) pullOCI(ctx context.Context, ociURL string) (*Manifest, []byte, []byte, string, error) {
	registry, repo, _, err := parseOCIURL(ociURL)
	if err != nil {
		return nil, nil, nil, "", status.Errorf(codes.InvalidArgument, "invalid oci_url: %v", err)
	}
	_ = repo
	auth := l.ociAuthForRegistry(registry)
	artifact, err := l.packager.PullFromRegistry(ctx, ociURL, auth)
	if err != nil {
		return nil, nil, nil, "", status.Errorf(codes.Internal, "oci pull failed: %v", err)
	}
	manifest, err := ParseManifest(artifact.Manifest)
	if err != nil {
		return nil, nil, nil, "", status.Errorf(codes.InvalidArgument, "invalid manifest from OCI: %v", err)
	}
	return manifest, artifact.WASM, artifact.Signature, artifact.KeyID, nil
}

// LoaderAdminGRPCServer adapts Loader to the pluginpb.PluginAdminServiceServer
// interface generated from proto/plugin/admin.proto.
type LoaderAdminGRPCServer struct {
	pluginpb.UnimplementedPluginAdminServiceServer
	Loader *Loader
}

// requireLeader returns a gRPC error if this Loader is not the Raft leader.
// Admin write operations must run on the leader; read operations may be served
// by followers (spec §12.3, P5-1).
//
// Callers that want transparent follower→leader forwarding should check
// IsLeader and use leaderAdminClient instead of returning this error.
func (s *LoaderAdminGRPCServer) requireLeader() error {
	if s.Loader.IsLeader() {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition, "not leader: %s", s.Loader.LeaderAddr())
}

// leaderAdminClient dials the current Raft leader's gRPC admin server.
// The returned close function must be called when the client is no longer
// needed. If no leader is known or the leader is unreachable, an error is
// returned (spec §12.3, P5-1).
func (s *LoaderAdminGRPCServer) leaderAdminClient(ctx context.Context) (pluginpb.PluginAdminServiceClient, func(), error) {
	addr := s.Loader.LeaderAdminAddr()
	if addr == "" {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "not leader: no known leader")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "failed to dial leader %s: %v", addr, err)
	}
	closeFn := func() { _ = conn.Close() }
	return pluginpb.NewPluginAdminServiceClient(conn), closeFn, nil
}

// InstallPlugin loads a plugin from raw bytes or an OCI URL (spec §3.16, P3-3).
func (s *LoaderAdminGRPCServer) InstallPlugin(ctx context.Context, req *pluginpb.InstallPluginRequest) (*pluginpb.InstallPluginResponse, error) {
	if !s.Loader.IsLeader() {
		client, closeFn, err := s.leaderAdminClient(ctx)
		if err != nil {
			return nil, err
		}
		defer closeFn()
		return client.InstallPlugin(ctx, req)
	}
	var (
		manifestBytes = req.ManifestBytes
		wasmBytes     = req.WasmBytes
		signature     = req.Signature
		keyID         = req.KeyId
	)

	if req.OciUrl != "" {
		m, w, sig, kid, err := s.Loader.pullOCI(ctx, req.OciUrl)
		if err != nil {
			return nil, err
		}
		manifestBytes, _ = json.Marshal(m)
		wasmBytes = w
		signature = sig
		keyID = kid
	}

	if len(manifestBytes) == 0 {
		return nil, status.Error(codes.InvalidArgument, "manifest_bytes is required")
	}
	if len(wasmBytes) == 0 {
		return nil, status.Error(codes.InvalidArgument, "wasm_bytes is required")
	}
	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid manifest: %v", err)
	}
	if err := s.Loader.InstallPlugin(ctx, manifest, wasmBytes, signature, keyID); err != nil {
		logger.AuditLogger("plugin.install", manifest.Name, "", "denied", map[string]interface{}{
			"version": manifest.Version,
			"reason":  err.Error(),
		})
		if IsErrRoutePrefixConflict(err) {
			return nil, status.Error(codes.AlreadyExists, err.Error())
		}
		if IsErrManifestInvalid(err) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "install failed: %v", err)
	}

	// Persist artifacts to local cache for reload/recovery.
	record, err := s.Loader.GetPluginRecord(manifest.Name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "install succeeded but record missing: %v", err)
	}
	if record == nil {
		return nil, status.Errorf(codes.Internal, "install succeeded but record not found")
	}
	_ = s.Loader.writePluginCache(record, manifestBytes, wasmBytes)

	logger.AuditLogger("plugin.install", manifest.Name, "", "allowed", map[string]interface{}{
		"version":    manifest.Version,
		"trust_tier": record.TrustTier,
		"key_id":     keyID,
	})
	return &pluginpb.InstallPluginResponse{
		PluginName: manifest.Name,
		Version:    manifest.Version,
		TrustTier:  int32(record.TrustTier),
	}, nil
}

// UninstallPlugin removes a plugin.
func (s *LoaderAdminGRPCServer) UninstallPlugin(ctx context.Context, req *pluginpb.UninstallPluginRequest) (*pluginpb.UninstallPluginResponse, error) {
	if !s.Loader.IsLeader() {
		client, closeFn, err := s.leaderAdminClient(ctx)
		if err != nil {
			return nil, err
		}
		defer closeFn()
		return client.UninstallPlugin(ctx, req)
	}
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}
	if err := s.Loader.UninstallPlugin(ctx, req.PluginName, req.Force); err != nil {
		logger.AuditLogger("plugin.uninstall", req.PluginName, "", "denied", map[string]interface{}{
			"force":  req.Force,
			"reason": err.Error(),
		})
		if IsErrHasDependents(err) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "uninstall failed: %v", err)
	}
	logger.AuditLogger("plugin.uninstall", req.PluginName, "", "allowed", map[string]interface{}{
		"force": req.Force,
	})
	return &pluginpb.UninstallPluginResponse{Uninstalled: true}, nil
}

// ReloadPlugin reinstalls a plugin from the local cache.
func (s *LoaderAdminGRPCServer) ReloadPlugin(ctx context.Context, req *pluginpb.ReloadPluginRequest) (*pluginpb.ReloadPluginResponse, error) {
	if !s.Loader.IsLeader() {
		client, closeFn, err := s.leaderAdminClient(ctx)
		if err != nil {
			return nil, err
		}
		defer closeFn()
		return client.ReloadPlugin(ctx, req)
	}
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}
	if err := s.Loader.ReloadPlugin(ctx, req.PluginName); err != nil {
		logger.AuditLogger("plugin.reload", req.PluginName, "", "denied", map[string]interface{}{
			"reason": err.Error(),
		})
		return nil, err
	}
	record, err := s.Loader.GetPluginRecord(req.PluginName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload succeeded but record missing: %v", err)
	}
	logger.AuditLogger("plugin.reload", req.PluginName, "", "allowed", map[string]interface{}{
		"version":    record.Version,
		"trust_tier": record.TrustTier,
	})
	return &pluginpb.ReloadPluginResponse{
		PluginName: record.Name,
		Version:    record.Version,
		TrustTier:  int32(record.TrustTier),
	}, nil
}

// ListPlugins returns installed plugins.
func (s *LoaderAdminGRPCServer) ListPlugins(ctx context.Context, _ *pluginpb.ListPluginsRequest) (*pluginpb.ListPluginsResponse, error) {
	records, err := s.Loader.ListPlugins(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list plugins: %v", err)
	}
	out := make([]*pluginpb.PluginInfo, 0, len(records))
	for _, r := range records {
		var m Manifest
		_ = json.Unmarshal(r.Manifest, &m)
		info := &pluginpb.PluginInfo{
			Name:        r.Name,
			Version:     r.Version,
			TrustTier:   int32(r.TrustTier),
			InstalledAt: r.InstalledAt,
			Capabilities: append([]string(nil), m.Capabilities...),
		}
		for _, route := range m.Routes {
			info.Routes = append(info.Routes, route.Prefix)
		}
		out = append(out, info)
	}
	return &pluginpb.ListPluginsResponse{Plugins: out}, nil
}

// GetPluginState reads a single key from a plugin's state namespace.
func (s *LoaderAdminGRPCServer) GetPluginState(ctx context.Context, req *pluginpb.GetPluginStateRequest) (*pluginpb.GetPluginStateResponse, error) {
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}
	entry, err := s.Loader.StateGet(req.PluginName, req.Key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "state get failed: %v", err)
	}
	if entry == nil {
		return &pluginpb.GetPluginStateResponse{Found: false}, nil
	}
	return &pluginpb.GetPluginStateResponse{
		Value:   entry.Value,
		Version: entry.Version,
		Found:   true,
	}, nil
}

// DumpPluginState exports the entire state:kv namespace of a plugin.
func (s *LoaderAdminGRPCServer) DumpPluginState(ctx context.Context, req *pluginpb.DumpPluginStateRequest) (*pluginpb.DumpPluginStateResponse, error) {
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}
	dump, err := s.Loader.StateDump(req.PluginName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "state dump failed: %v", err)
	}
	entries := make([]*pluginpb.StateKVEntry, 0, len(dump))
	for k, e := range dump {
		entries = append(entries, &pluginpb.StateKVEntry{
			Key:     k,
			Value:   e.Value,
			Version: e.Version,
		})
	}
	return &pluginpb.DumpPluginStateResponse{
		PluginName: req.PluginName,
		Entries:    entries,
	}, nil
}

// GetPluginLogs returns recent log entries emitted by a plugin.
func (s *LoaderAdminGRPCServer) GetPluginLogs(ctx context.Context, req *pluginpb.GetPluginLogsRequest) (*pluginpb.GetPluginLogsResponse, error) {
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 100
	}
	logs := s.Loader.RecentDebugLogs(req.PluginName, limit)
	entries := make([]*pluginpb.PluginLogEntry, len(logs))
	for i, e := range logs {
		entries[i] = &pluginpb.PluginLogEntry{
			Level:       e.Level,
			Payload:     e.Payload,
			TimestampMs: e.Timestamp.UnixMilli(),
		}
	}
	return &pluginpb.GetPluginLogsResponse{
		PluginName: req.PluginName,
		Entries:    entries,
	}, nil
}

// GetPluginMetrics returns recent metric samples recorded by a plugin.
func (s *LoaderAdminGRPCServer) GetPluginMetrics(ctx context.Context, req *pluginpb.GetPluginMetricsRequest) (*pluginpb.GetPluginMetricsResponse, error) {
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 100
	}
	metrics := s.Loader.RecentDebugMetrics(req.PluginName, limit)
	samples := make([]*pluginpb.PluginMetricSample, len(metrics))
	for i, m := range metrics {
		samples[i] = &pluginpb.PluginMetricSample{
			Name:        m.Name,
			Value:       m.Value,
			LabelsJson:  m.Labels,
			TimestampMs: m.Timestamp.UnixMilli(),
		}
	}
	return &pluginpb.GetPluginMetricsResponse{
		PluginName: req.PluginName,
		Samples:    samples,
	}, nil
}

// SetPluginLogLevel adjusts the per-plugin log level dynamically.
func (s *LoaderAdminGRPCServer) SetPluginLogLevel(ctx context.Context, req *pluginpb.SetPluginLogLevelRequest) (*pluginpb.SetPluginLogLevelResponse, error) {
	if !s.Loader.IsLeader() {
		client, closeFn, err := s.leaderAdminClient(ctx)
		if err != nil {
			return nil, err
		}
		defer closeFn()
		return client.SetPluginLogLevel(ctx, req)
	}
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}
	level := s.Loader.SetPluginLogLevel(req.PluginName, req.Level)
	logger.AuditLogger("plugin.log_level.set", req.PluginName, "", "allowed", map[string]interface{}{
		"level": level,
	})
	return &pluginpb.SetPluginLogLevelResponse{
		PluginName: req.PluginName,
		Level:      level,
	}, nil
}

// AddTrustedKey adds an Ed25519 public key to the replicated trust store
// and persists it back to config/trusted_keys.json.
func (s *LoaderAdminGRPCServer) AddTrustedKey(ctx context.Context, req *pluginpb.AddTrustedKeyRequest) (*pluginpb.AddTrustedKeyResponse, error) {
	if !s.Loader.IsLeader() {
		client, closeFn, err := s.leaderAdminClient(ctx)
		if err != nil {
			return nil, err
		}
		defer closeFn()
		return client.AddTrustedKey(ctx, req)
	}
	if req.KeyId == "" {
		return nil, status.Error(codes.InvalidArgument, "key_id is required")
	}
	if req.PublicKey == "" {
		return nil, status.Error(codes.InvalidArgument, "public_key is required")
	}
	if err := s.Loader.AddTrustedKey(ctx, req.KeyId, req.PublicKey, req.IsCore, req.AddedBy); err != nil {
		logger.AuditLogger("trusted_key.add", req.KeyId, "", "denied", map[string]interface{}{
			"reason": err.Error(),
		})
		return nil, status.Errorf(codes.Internal, "failed to add trusted key: %v", err)
	}
	logger.AuditLogger("trusted_key.add", req.KeyId, "", "allowed", map[string]interface{}{
		"is_core":  req.IsCore,
		"added_by": req.AddedBy,
	})
	return &pluginpb.AddTrustedKeyResponse{KeyId: req.KeyId, Added: true}, nil
}

// RemoveTrustedKey deletes a trusted key from the replicated trust store.
func (s *LoaderAdminGRPCServer) RemoveTrustedKey(ctx context.Context, req *pluginpb.RemoveTrustedKeyRequest) (*pluginpb.RemoveTrustedKeyResponse, error) {
	if !s.Loader.IsLeader() {
		client, closeFn, err := s.leaderAdminClient(ctx)
		if err != nil {
			return nil, err
		}
		defer closeFn()
		return client.RemoveTrustedKey(ctx, req)
	}
	if req.KeyId == "" {
		return nil, status.Error(codes.InvalidArgument, "key_id is required")
	}
	if err := s.Loader.RemoveTrustedKey(ctx, req.KeyId); err != nil {
		logger.AuditLogger("trusted_key.remove", req.KeyId, "", "denied", map[string]interface{}{
			"reason": err.Error(),
		})
		return nil, status.Errorf(codes.Internal, "failed to remove trusted key: %v", err)
	}
	logger.AuditLogger("trusted_key.remove", req.KeyId, "", "allowed", nil)
	return &pluginpb.RemoveTrustedKeyResponse{KeyId: req.KeyId, Removed: true}, nil
}

// ListTrustedKeys returns the current trusted keys mirror.
func (s *LoaderAdminGRPCServer) ListTrustedKeys(ctx context.Context, _ *pluginpb.ListTrustedKeysRequest) (*pluginpb.ListTrustedKeysResponse, error) {
	keys := s.Loader.ListTrustedKeys()
	out := make([]*pluginpb.TrustedKeyInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, &pluginpb.TrustedKeyInfo{
			KeyId:     k.KeyID,
			PublicKey: k.PublicKey,
			IsCore:    k.IsCore,
			AddedAt:   k.AddedAt,
			AddedBy:   k.AddedBy,
		})
	}
	return &pluginpb.ListTrustedKeysResponse{Keys: out}, nil
}

// IsErrRoutePrefixConflict reports whether err is a route prefix conflict.
func IsErrRoutePrefixConflict(err error) bool {
	var e ErrRoutePrefixConflict
	return errors.As(err, &e)
}

// AddTrustedKey registers a trusted Ed25519 public key through Raft and
// persists the updated key set back to config/trusted_keys.json.
func (l *Loader) AddTrustedKey(ctx context.Context, keyID, publicKey string, isCore bool, addedBy string) error {
	key := TrustedKey{
		KeyID:     keyID,
		PublicKey: publicKey,
		IsCore:    isCore,
		AddedAt:   time.Now().UTC().Format(time.RFC3339),
		AddedBy:   addedBy,
	}
	if err := l.trust.AddKey(key); err != nil {
		return err
	}
	keyJSON, err := json.Marshal(key)
	if err != nil {
		return fmt.Errorf("failed to marshal trusted key: %w", err)
	}
	op := &LoaderFSMOp{
		Type: OpTrustedKeyAdd,
		Key:  keyID,
		Data: keyJSON,
	}
	if err := l.RaftApply(ctx, op); err != nil {
		l.trust.RemoveKey(keyID)
		return fmt.Errorf("failed to replicate trusted key add: %w", err)
	}
	if err := l.persistTrustedKeysFile(); err != nil {
		// The key is already replicated; the file is a best-effort mirror.
		zap.L().Warn("failed to persist trusted_keys.json", zap.Error(err))
	}
	return nil
}

// RemoveTrustedKey deletes a trusted key through Raft and persists the
// updated key set back to config/trusted_keys.json.
func (l *Loader) RemoveTrustedKey(ctx context.Context, keyID string) error {
	existed := l.trust.RemoveKey(keyID)
	op := &LoaderFSMOp{
		Type: OpTrustedKeyRemove,
		Key:  keyID,
	}
	if err := l.RaftApply(ctx, op); err != nil {
		// Best-effort rollback of the in-memory store.
		if existed {
			_ = l.restoreTrustedKeyFromFSM(keyID)
		}
		return fmt.Errorf("failed to replicate trusted key remove: %w", err)
	}
	if err := l.persistTrustedKeysFile(); err != nil {
		zap.L().Warn("failed to persist trusted_keys.json", zap.Error(err))
	}
	return nil
}

// ListTrustedKeys returns the current trusted keys from the in-memory store.
func (l *Loader) ListTrustedKeys() []TrustedKey {
	return l.trust.Keys()
}

// persistTrustedKeysFile writes the current in-memory trusted keys to the
// configured trusted_keys.json path.
func (l *Loader) persistTrustedKeysFile() error {
	path := l.cfg.TrustedKeysPath
	if path == "" {
		return nil
	}
	keys := l.trust.Keys()
	data, err := MarshalTrustedKeysFile(keys)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	return nil
}

// restoreTrustedKeyFromFSM reloads a single key from the FSM mirror into the
// in-memory trust store. Used for rollback after a failed remove.
func (l *Loader) restoreTrustedKeyFromFSM(keyID string) error {
	db := l.fsm.DB()
	if db == nil {
		return errors.New("loader FSM db is nil")
	}
	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTrustedKeys))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(keyID))
		if raw == nil {
			return nil
		}
		var key TrustedKey
		if err := json.Unmarshal(raw, &key); err != nil {
			return fmt.Errorf("corrupt trusted key %s: %w", keyID, err)
		}
		_ = l.trust.AddKey(key)
		return nil
	})
}

// Ensure LoaderAdminGRPCServer implements the interface.
var _ pluginpb.PluginAdminServiceServer = (*LoaderAdminGRPCServer)(nil)

// Ensure config import is used.
var _ = config.PluginLoaderConfig{}
