package main

import (
	"flag"

	"go.uber.org/zap"

	"cipherlake/internal/services/keystore_service"
	"cipherlake/internal/services/server"
)

func main() {
	port := flag.Int("port", 50056, "gRPC listen port")
	dataPath := flag.String("data-path", "./data/keystore", "Path to keystore data directory")
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

	svc, err := keystore_service.NewKeyStoreService(keystore_service.KeyStoreServiceConfig{
		DataPath: *dataPath,
	})
	if err != nil {
		logger.Fatal("failed to create keystore service", zap.Error(err))
	}

	grpcSrv := keystore_service.NewGRPCServer(svc)

	if err := server.StartService(server.ServiceConfig{
		Name:        "keystore-service",
		Port:        *port,
		ConsulAddr:  *consulAddr,
		ServiceAddr: *serviceAddr,
		TTLSeconds:  *ttlSeconds,
	}, grpcSrv); err != nil {
		logger.Fatal("keystore service failed", zap.Error(err))
	}
}
