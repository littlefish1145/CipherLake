package main

import (
	"flag"

	"go.uber.org/zap"

	"cipherlake/internal/services/decrypt_service"
	"cipherlake/internal/services/server"
)

func main() {
	port := flag.Int("port", 50055, "gRPC listen port")
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

	svc := decrypt_service.NewDecryptService(decrypt_service.DecryptServiceConfig{})

	grpcSrv := decrypt_service.NewGRPCServer(svc)

	if err := server.StartService(server.ServiceConfig{
		Name:        "decrypt-service",
		Port:        *port,
		ConsulAddr:  *consulAddr,
		ServiceAddr: *serviceAddr,
		TTLSeconds:  *ttlSeconds,
	}, grpcSrv); err != nil {
		logger.Fatal("decrypt service failed", zap.Error(err))
	}
}
