package main

import (
	"flag"

	"go.uber.org/zap"

	"cipherlake/internal/services/encrypt_service"
	"cipherlake/internal/services/server"
)

func main() {
	port := flag.Int("port", 50054, "gRPC listen port")
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

	svc := encrypt_service.NewEncryptService(encrypt_service.EncryptServiceConfig{})

	grpcSrv := encrypt_service.NewGRPCServer(svc)

	if err := server.StartService(server.ServiceConfig{
		Name:        "encrypt-service",
		Port:        *port,
		ConsulAddr:  *consulAddr,
		ServiceAddr: *serviceAddr,
		TTLSeconds:  *ttlSeconds,
	}, grpcSrv); err != nil {
		logger.Fatal("encrypt service failed", zap.Error(err))
	}
}
