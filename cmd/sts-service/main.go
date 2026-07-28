package main

import (
	"flag"
	"os"
	"time"

	"cipherlake/internal/iam"
	"cipherlake/internal/services/server"
	"cipherlake/internal/services/sts_service"

	pb "cipherlake/proto/sts"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func main() {
	port := flag.Int("port", 50057, "gRPC listen port")
	iamDBPath := flag.String("iam-db", "./data/iam.db", "Path to IAM BoltDB database")
	masterKeyPath := flag.String("master-key", "./data/master.key", "Path to master key file")
	flag.Parse()

	// Initialize logger
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	zap.ReplaceGlobals(logger)

	// Initialize master key
	masterKey, err := iam.NewMasterKey(*masterKeyPath)
	if err != nil {
		zap.L().Fatal("failed to initialize master key", zap.Error(err))
	}
	defer masterKey.Zero()

	// Initialize IAM store
	iamStore, err := iam.NewIAMStore(*iamDBPath)
	if err != nil {
		zap.L().Fatal("failed to initialize IAM store", zap.Error(err))
	}
	defer iamStore.Close()

	// Initialize IAM service
	iamService := iam.NewIAMService(iamStore, masterKey)

	// Initialize STS service
	stsSvc := sts_service.NewSTSService(iamService)
	stsSvc.StartCleanupLoop(5 * time.Minute)

	// Create gRPC adapter
	adapter := sts_service.NewGRPCAdapter(stsSvc)

	// Start gRPC server using common server package
	cfg := server.ServiceConfig{
		Name: "sts-service",
		Port: *port,
	}

	srv := &stsGRPCService{
		adapter: adapter,
	}

	if err := server.StartService(cfg, srv); err != nil {
		zap.L().Fatal("failed to start STS service", zap.Error(err))
		os.Exit(1)
	}
}

// stsGRPCService wraps the adapter to implement the server.ServiceServer interface
type stsGRPCService struct {
	adapter *sts_service.GRPCAdapter
}

func (s *stsGRPCService) Register(srv *grpc.Server) error {
	pb.RegisterSTSServiceServer(srv, s.adapter)
	return nil
}

func (s *stsGRPCService) Close() error { return nil }
