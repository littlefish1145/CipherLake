package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"cipherlake/internal/config"
	"cipherlake/internal/gateway"
	"cipherlake/internal/iam"
	"cipherlake/internal/logger"
	"cipherlake/internal/tiering"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	configPath string
	port       string
	verbose    bool
)

var rootCmd = &cobra.Command{
	Use:   "cipherlake",
	Short: "CipherLake - Intelligent S3-compatible storage system",
	Long: `CipherLake is a next-generation S3-compatible storage system with:
- Intelligent tiering (Hot/Warm/Cold/Archive)
- Zero-trust encryption with external KMS
- Native vector search capability
- Content processing pipelines`,
	Run: func(cmd *cobra.Command, args []string) {
		runServer()
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "config.yaml", "Path to configuration file")
	rootCmd.PersistentFlags().StringVar(&port, "port", ":8080", "Port to listen on")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runServer() {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	if err := logger.Init(&logger.Config{
		Level:      cfg.Logging.Level,
		Format:     cfg.Logging.Format,
		OutputPath: cfg.Logging.OutputPath,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	if port != "" && port != ":8080" {
		cfg.Node.ListenAddr = port
	} else if cfg.Node.ListenAddr == "" {
		cfg.Node.ListenAddr = ":8080"
	}

	if verbose {
		cfg.Logging.Level = "debug"
	}

	logger.Info("Starting CipherLake S3 Gateway",
		zap.String("addr", cfg.Node.ListenAddr),
		zap.String("data_dir", cfg.Node.DataDir),
		zap.Bool("tiering", cfg.Tiering.Enabled),
		zap.Bool("vector", cfg.Vector.Enabled),
		zap.Bool("encryption_dedup", cfg.Encryption.EnableDedup),
		zap.Bool("auth_required", cfg.Auth.RequireAuth),
		zap.Bool("tls", cfg.TLS.Enabled),
		zap.Bool("ratelimit", cfg.RateLimit.Enabled),
	)

	gw, err := gateway.NewS3Gateway(cfg)
	if err != nil {
		logger.Fatal("Failed to create gateway", zap.Error(err))
	}

	mux := http.NewServeMux()

	// Initialize IAM system if enabled
	if cfg.IAM.Enabled {
		var iamProvider iam.IAMServiceProvider

		if cfg.IAM.STSServiceAddr != "" {
			// Distributed mode: use gRPC to sts-service for IAM lookups
			// This avoids opening the BoltDB database which is already
			// held by sts-service (BoltDB only allows one writer at a time)
			logger.Info("Using remote IAM service via gRPC", zap.String("addr", cfg.IAM.STSServiceAddr))

			remoteIAM, err := iam.NewRemoteIAMService(cfg.IAM.STSServiceAddr)
			if err != nil {
				logger.Fatal("Failed to connect to remote IAM service", zap.Error(err))
			}
			defer remoteIAM.Close()
			iamProvider = remoteIAM

			// Mount IAM Admin API via gRPC proxy to sts-service
			remoteAdminAPI := iam.NewRemoteAdminAPI(remoteIAM.Client(), []byte("cipherlake-iam-jwt-key"))
			mux.Handle("/iam/", http.StripPrefix("/iam", remoteAdminAPI))
		} else {
			// Standalone mode: open the IAM database directly
			masterKey, err := iam.NewMasterKey(cfg.IAM.MasterKeyPath)
			if err != nil {
				logger.Fatal("Failed to initialize IAM master key", zap.Error(err))
			}

			iamStore, err := iam.NewIAMStore(cfg.IAM.DBPath)
			if err != nil {
				logger.Fatal("Failed to initialize IAM store", zap.Error(err))
			}

			iamService := iam.NewIAMService(iamStore, masterKey)

			// Initialize admin user (first run only)
			adminKey, err := iamService.InitializeAdmin()
			if err != nil {
				logger.Error("Failed to initialize admin user", zap.Error(err))
			}
			if adminKey != nil {
				fmt.Println("==============================================")
				fmt.Println("  CIPHERLAKE ADMIN ACCESS KEY (SHOW ONCE)")
				fmt.Printf("  Access Key ID:     %s\n", adminKey.AccessKeyID)
				fmt.Printf("  Secret Access Key: %s\n", adminKey.SecretAccessKey)
				fmt.Println("  WARNING: This will NOT be shown again!")
				fmt.Println("  Store these credentials securely.")
				fmt.Println("==============================================")
			}

			iamProvider = iamService

			// Mount IAM Admin API (only available in standalone mode)
			iamAdminAPI := iam.NewAdminAPI(iamService, []byte("cipherlake-iam-jwt-key"))
			mux.Handle("/iam/", http.StripPrefix("/iam", iamAdminAPI))
		}

		// Create IAM bridge
		iamBridge := gateway.NewIAMAuthBridge(iamProvider, gw.GetAuth())
		gw.GetAuth().SetIAMBridge(iamBridge)
		gw.SetIAMBridge(iamBridge)
	}

	adminAPI := gateway.NewAdminAPI(gw, gw.GetAuth())

	mux.Handle("/", gw.Handler())
	mux.Handle("/admin/", adminAPI.Handler())
	mux.HandleFunc("/health", healthHandler(gw))
	mux.HandleFunc("/ready", readyHandler(gw))

	tieringCtx, tieringCancel := context.WithCancel(context.Background())
	go runTieringScheduler(tieringCtx, gw.GetTieringManager())

	var handler http.Handler = mux
	handler = gateway.SecurityHeadersMiddleware(handler)

	var tlsManager *gateway.TLSManager
	if cfg.TLS.Enabled {
		tlsManager, err = gateway.NewTLSManager(&cfg.TLS)
		if err != nil {
			logger.Fatal("Failed to initialize TLS manager", zap.Error(err))
		}

		handler = gateway.HSTSMiddleware(handler)

		tlsManager.StartCertWatcher()

		if err := tlsManager.StartHTTPRedirect(cfg.Node.ListenAddr); err != nil {
			logger.Error("Failed to start HTTP redirect server", zap.Error(err))
		}
	}

	server := &http.Server{
		Addr:         cfg.Node.ListenAddr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if cfg.TLS.Enabled && tlsManager != nil {
		server.TLSConfig = tlsManager.GetTLSConfig()
	}

	go func() {
		var err error
		if cfg.TLS.Enabled {
			logger.Info("Starting HTTPS server with TLS")
			err = server.ListenAndServeTLS("", "")
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Fatal("Server failed", zap.Error(err))
		}
	}()

	logger.Info("CipherLake server started successfully",
		zap.Bool("tls", cfg.TLS.Enabled),
		zap.Bool("ratelimit", cfg.RateLimit.Enabled),
	)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tieringCancel()

	if err := gw.Close(); err != nil {
		logger.Error("Failed to close gateway", zap.Error(err))
	}

	if tlsManager != nil {
		tlsManager.Stop()
	}

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("Server forced to shutdown", zap.Error(err))
	}

	logger.Info("Server stopped")
}

func runTieringScheduler(ctx context.Context, tm *tiering.TieringManager) {
	if tm == nil {
		return
	}
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logger.Info("Running tiering decision cycle")
			decisions, err := tm.RunTieringDecision(ctx)
			if err != nil {
				logger.Error("Tiering decision failed", zap.Error(err))
				continue
			}
			if len(decisions) > 0 {
				logger.Info("Executing tiering migrations", zap.Int("count", len(decisions)))
				if err := tm.ExecuteMigrations(ctx, decisions); err != nil {
					logger.Error("Tiering migration failed", zap.Error(err))
				}
			}
		}
	}
}

func healthHandler(gw *gateway.S3Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		health := map[string]interface{}{
			"status":    "healthy",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}

		checks := make(map[string]interface{})

		if gw.GetMetadataStore() != nil {
			checks["metadata"] = "ok"
		} else {
			checks["metadata"] = "unavailable"
			health["status"] = "degraded"
		}

		if gw.GetStore() != nil {
			checks["storage"] = "ok"
		} else {
			checks["storage"] = "unavailable"
			health["status"] = "unhealthy"
		}

		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		checks["memory_mb"] = m.Alloc / 1024 / 1024
		checks["goroutines"] = runtime.NumGoroutine()

		health["checks"] = checks

		w.Header().Set("Content-Type", "application/json")
		if health["status"] == "unhealthy" {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		json.NewEncoder(w).Encode(health)
	}
}

func readyHandler(gw *gateway.S3Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ready := true
		reasons := []string{}
		checks := make(map[string]string)

		if gw.GetMetadataStore() == nil {
			ready = false
			reasons = append(reasons, "metadata store not initialized")
			checks["metadata"] = "not_ready"
		} else {
			checks["metadata"] = "ready"
		}

		if gw.GetStore() == nil {
			ready = false
			reasons = append(reasons, "storage not initialized")
			checks["storage"] = "not_ready"
		} else {
			checks["storage"] = "ready"
		}

		response := map[string]interface{}{
			"ready":   ready,
			"checks":  checks,
			"reasons": reasons,
		}

		w.Header().Set("Content-Type", "application/json")
		if ready {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(response)
	}
}
