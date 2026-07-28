package keystore_service

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"cipherlake/internal/services"
	"cipherlake/internal/services/server"
	common "cipherlake/proto/common"
	pb "cipherlake/proto/keystore"
)

// GRPCServer adapts KeyStoreService to gRPC interface
type GRPCServer struct {
	pb.UnimplementedKeyStoreServiceServer
	service *KeyStoreService
}

// NewGRPCServer creates a new gRPC server adapter
func NewGRPCServer(service *KeyStoreService) *GRPCServer {
	return &GRPCServer{service: service}
}

// Register registers the service with a gRPC server
func (g *GRPCServer) Register(grpcServer *grpc.Server) error {
	pb.RegisterKeyStoreServiceServer(grpcServer, g)
	return nil
}

// Close closes the underlying service
func (g *GRPCServer) Close() error {
	return g.service.Close()
}

// StoreKey handles gRPC request
func (g *GRPCServer) StoreKey(ctx context.Context, req *pb.StoreKeyRequest) (*pb.StoreKeyResponse, error) {
	encryptedDEK := protoToEncryptedDEK(req.GetEncryptedDek())

	keyID, err := g.service.StoreKey(
		req.GetBucket(),
		req.GetObjectKey(),
		encryptedDEK,
		req.GetObjectSize(),
	)
	if err != nil {
		zap.L().Error("failed to store key", zap.Error(err))
		return &pb.StoreKeyResponse{
			Error: &common.Error{Code: 500, Message: err.Error()},
		}, nil
	}

	return &pb.StoreKeyResponse{
		KeyId:    keyID,
		StoredAt: time.Now().Unix(),
	}, nil
}

// GetKey handles gRPC request
func (g *GRPCServer) GetKey(ctx context.Context, req *pb.GetKeyRequest) (*pb.GetKeyResponse, error) {
	encryptedDEK, err := g.service.GetKey(
		req.GetBucket(),
		req.GetObjectKey(),
	)
	if err != nil {
		zap.L().Error("failed to get key", zap.Error(err))
		return &pb.GetKeyResponse{
			Error: &common.Error{Code: 404, Message: err.Error()},
		}, nil
	}

	return &pb.GetKeyResponse{
		EncryptedDek: encryptedDEKToProto(encryptedDEK),
		KeyId:        encryptedDEK.KeyID,
		Algorithm:    encryptedDEK.Algorithm,
	}, nil
}

// DeleteKey handles gRPC request
func (g *GRPCServer) DeleteKey(ctx context.Context, req *pb.DeleteKeyRequest) (*pb.DeleteKeyResponse, error) {
	err := g.service.DeleteKey(
		req.GetBucket(),
		req.GetObjectKey(),
	)
	if err != nil {
		zap.L().Error("failed to delete key", zap.Error(err))
		return &pb.DeleteKeyResponse{
			Error: &common.Error{Code: 500, Message: err.Error()},
		}, nil
	}

	return &pb.DeleteKeyResponse{
		Deleted:      true,
		DeletedCount: 1,
	}, nil
}

// ListKeys handles gRPC request
func (g *GRPCServer) ListKeys(ctx context.Context, req *pb.ListKeysRequest) (*pb.ListKeysResponse, error) {
	entries, total, err := g.service.ListKeys(
		req.GetBucket(),
		req.GetPrefix(),
		int(req.GetLimit()),
		int(req.GetOffset()),
	)
	if err != nil {
		zap.L().Error("failed to list keys", zap.Error(err))
		return &pb.ListKeysResponse{
			Error: &common.Error{Code: 500, Message: err.Error()},
		}, nil
	}

	pbEntries := make([]*pb.KeyEntry, len(entries))
	for i, entry := range entries {
		pbEntries[i] = &pb.KeyEntry{
			ObjectKey: entry.ObjectKey,
			KeyId:     entry.KeyID,
			CreatedAt: entry.CreatedAt.Unix(),
			Algorithm: entry.Algorithm,
		}
	}

	return &pb.ListKeysResponse{
		Keys:       pbEntries,
		TotalCount: int32(total),
	}, nil
}

// Health handles gRPC health check
func (g *GRPCServer) Health(ctx context.Context, req *pb.HealthRequest) (*pb.HealthResponse, error) {
	stats := g.service.GetStats()
	totalKeys := int64(0)
	if v, ok := stats["total_keys"].(int); ok {
		totalKeys = int64(v)
	}

	return &pb.HealthResponse{
		Healthy:         true,
		ServiceName:     "keystore-service",
		Version:         "1.0.0",
		TotalKeysStored: totalKeys,
	}, nil
}

func protoToEncryptedDEK(p *common.EncryptedDEK) *services.EncryptedDEK {
	if p == nil {
		return nil
	}
	return &services.EncryptedDEK{
		EncryptedKey: p.EncryptedKey,
		Algorithm:    p.Algorithm,
		KeyID:        p.KeyId,
		KeyVersion:   int(p.KeyVersion),
	}
}

func encryptedDEKToProto(d *services.EncryptedDEK) *common.EncryptedDEK {
	if d == nil {
		return nil
	}
	return &common.EncryptedDEK{
		EncryptedKey: d.EncryptedKey,
		Algorithm:    d.Algorithm,
		KeyId:        d.KeyID,
		KeyVersion:   int32(d.KeyVersion),
	}
}

var _ server.ServiceServer = (*GRPCServer)(nil)
