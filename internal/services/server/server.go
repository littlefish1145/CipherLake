package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"cipherlake/internal/services"
)

// ServiceConfig holds common configuration for a gRPC service
type ServiceConfig struct {
	Name        string
	Port        int
	TLSConfig   *tls.Config
	ConsulAddr  string
	ServiceAddr string
	TTLSeconds  int
}

// ServiceServer is the interface each service must implement
type ServiceServer interface {
	Register(grpcServer *grpc.Server) error
	Close() error
}

// StartService starts a gRPC service with common infrastructure
func StartService(cfg ServiceConfig, srv ServiceServer) error {
	// Setup listener
	addr := fmt.Sprintf(":%d", cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	// Create gRPC server options
	var opts []grpc.ServerOption
	if cfg.TLSConfig != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(cfg.TLSConfig)))
	}

	grpcServer := grpc.NewServer(opts...)

	// Register service
	if err := srv.Register(grpcServer); err != nil {
		return fmt.Errorf("failed to register service: %w", err)
	}

	// Register health check
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus(cfg.Name, grpc_health_v1.HealthCheckResponse_SERVING)

	// Register reflection for debugging
	reflection.Register(grpcServer)

	// Register with Consul
	var registry *services.ServiceRegistry
	if cfg.ConsulAddr != "" {
		registry, err = services.NewServiceRegistry(cfg.ConsulAddr)
		if err != nil {
			zap.L().Warn("failed to connect to consul, running without service discovery", zap.Error(err))
		} else {
			ttl := time.Duration(cfg.TTLSeconds) * time.Second
			if ttl == 0 {
				ttl = 30 * time.Second
			}
			serviceAddr := cfg.ServiceAddr
			if serviceAddr == "" {
				serviceAddr = "127.0.0.1"
			}
			err = registry.Register(context.Background(), services.ServiceConfig{
				Name:          cfg.Name,
				Address:       serviceAddr,
				Port:          cfg.Port,
				TTL:           ttl,
				CheckInterval: 10 * time.Second,
			})
			if err != nil {
				zap.L().Warn("failed to register with consul", zap.Error(err))
			}
		}
	}

	// Handle shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		zap.L().Info("shutting down service", zap.String("service", cfg.Name))
		healthServer.SetServingStatus(cfg.Name, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		grpcServer.GracefulStop()
		if registry != nil {
			registry.Deregister(context.Background())
		}
		srv.Close()
	}()

	zap.L().Info("starting gRPC service",
		zap.String("service", cfg.Name),
		zap.String("addr", addr),
		zap.Bool("tls_enabled", cfg.TLSConfig != nil))

	return grpcServer.Serve(listener)
}
