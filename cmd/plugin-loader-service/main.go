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
// Phase 1 scope (spec Phase 1, P1-1):
//   - Construct Loader with Raft single-node bootstrap
//   - Start a gRPC server (placeholder service, real RPCs added in P1-4/P1-5)
//   - Healthz / readyz endpoints
//
// See .trae/specs/plugin-system/spec.txt for the full design.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"cipherlake/internal/config"
	"cipherlake/internal/pluginloader"
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

	// Start the gRPC server on GRPCListenAddr.
	grpcLn, err := net.Listen("tcp", cfg.PluginLoader.GRPCListenAddr)
	if err != nil {
		logger.Fatal("failed to listen for grpc", zap.String("addr", cfg.PluginLoader.GRPCListenAddr), zap.Error(err))
	}
	defer grpcLn.Close()

	grpcServer := grpc.NewServer()
	pluginpb.RegisterPluginLoaderServiceServer(grpcServer, &pluginloader.LoaderGRPCServer{Loader: loader})

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

		httpSrv = &http.Server{
			Addr:              cfg.PluginLoader.AdminListenAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
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
