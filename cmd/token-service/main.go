package main

import (
	"flag"

	"go.uber.org/zap"

	"cipherlake/internal/services/server"
	"cipherlake/internal/services/token_service"
)

func main() {
	port := flag.Int("port", 50051, "gRPC listen port")
	keyPath := flag.String("key-path", "./data/keys/token", "Path to Ed25519 signing key")
	consulAddr := flag.String("consul", "", "Consul address for service discovery")
	serviceAddr := flag.String("service-addr", "", "Service address for Consul registration")
	ttlSeconds := flag.Int("ttl", 30, "TTL seconds for Consul health check")
	flag.Parse()

	logger, err := zap.NewProduction()
	if err != nil {
		panic("failed to initialize logger: " + err.Error())
	}
	zap.ReplaceGlobals(logger)
	defer logger.Sync()

	svc, err := token_service.NewTokenService(token_service.TokenServiceConfig{
		KeyPath: *keyPath,
	})
	if err != nil {
		logger.Fatal("failed to create token service", zap.Error(err))
	}

	grpcSrv := token_service.NewGRPCServer(svc)

	if err := server.StartService(server.ServiceConfig{
		Name:        "token-service",
		Port:        *port,
		ConsulAddr:  *consulAddr,
		ServiceAddr: *serviceAddr,
		TTLSeconds:  *ttlSeconds,
	}, grpcSrv); err != nil {
		logger.Fatal("token service failed", zap.Error(err))
	}
}
