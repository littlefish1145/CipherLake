package main

import (
	"flag"

	"go.uber.org/zap"

	"cipherlake/internal/services/keygen_service"
	"cipherlake/internal/services/server"
)

func main() {
	port := flag.Int("port", 50052, "gRPC listen port")
	keyPath := flag.String("key-path", "./data/keys/keygen", "Path to EC key pair")
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

	svc, err := keygen_service.NewKeyGenService(keygen_service.KeyGenServiceConfig{
		KeyPath: *keyPath,
	})
	if err != nil {
		logger.Fatal("failed to create keygen service", zap.Error(err))
	}

	grpcSrv := keygen_service.NewGRPCServer(svc)

	if err := server.StartService(server.ServiceConfig{
		Name:        "keygen-service",
		Port:        *port,
		ConsulAddr:  *consulAddr,
		ServiceAddr: *serviceAddr,
		TTLSeconds:  *ttlSeconds,
	}, grpcSrv); err != nil {
		logger.Fatal("keygen service failed", zap.Error(err))
	}
}
