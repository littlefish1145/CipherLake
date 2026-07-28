package keygen_service

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"cipherlake/internal/services"
	"cipherlake/internal/services/server"
	common "cipherlake/proto/common"
	pb "cipherlake/proto/keygen"
)

// GRPCServer adapts KeyGenService to gRPC interface
type GRPCServer struct {
	pb.UnimplementedKeyGenServiceServer
	service *KeyGenService
}

// NewGRPCServer creates a new gRPC server adapter
func NewGRPCServer(service *KeyGenService) *GRPCServer {
	return &GRPCServer{service: service}
}

// Register registers the service with a gRPC server
func (g *GRPCServer) Register(grpcServer *grpc.Server) error {
	pb.RegisterKeyGenServiceServer(grpcServer, g)
	return nil
}

// Close closes the underlying service
func (g *GRPCServer) Close() error {
	return g.service.Close()
}

// GenerateDataKey handles gRPC request
func (g *GRPCServer) GenerateDataKey(ctx context.Context, req *pb.GenerateDataKeyRequest) (*pb.GenerateDataKeyResponse, error) {
	token := req.GetToken()
	if token == nil {
		return &pb.GenerateDataKeyResponse{
			Error: &common.Error{Code: 400, Message: "token is required"},
		}, nil
	}

	clientECDHPub := protoToECDHPublicKey(req.GetClientEcdhPub())

	encryptedDEK, ecdhEncryptedDEK, serviceECDHPub, err := g.service.GenerateDataKey(
		ctx,
		token.GetTokenId(),
		token.GetUserId(),
		token.GetBucket(),
		token.GetObjectKey(),
		clientECDHPub,
	)
	if err != nil {
		zap.L().Error("failed to generate data key", zap.Error(err))
		return &pb.GenerateDataKeyResponse{
			Error: &common.Error{Code: 500, Message: err.Error()},
		}, nil
	}

	return &pb.GenerateDataKeyResponse{
		EncryptedDek:     encryptedDEKToProto(encryptedDEK),
		EcdhEncryptedDek: ecdhEncryptedDEKToProto(ecdhEncryptedDEK),
		ServiceEcdhPub:   ecdhPublicKeyToProto(serviceECDHPub),
		KeyId:            encryptedDEK.KeyID,
		Algorithm:        encryptedDEK.Algorithm,
	}, nil
}

// GetPublicKey handles gRPC request
func (g *GRPCServer) GetPublicKey(ctx context.Context, req *pb.GetPublicKeyRequest) (*pb.GetPublicKeyResponse, error) {
	publicKey, keyID, algorithm := g.service.GetPublicKey()
	return &pb.GetPublicKeyResponse{
		PublicKey: publicKey,
		KeyId:     keyID,
		Algorithm: algorithm,
	}, nil
}

// Health handles gRPC health check
func (g *GRPCServer) Health(ctx context.Context, req *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{
		Healthy:     true,
		ServiceName: "keygen-service",
		Version:     "1.0.0",
	}, nil
}

func protoToECDHPublicKey(p *common.ECDHPublicKey) *services.ECDHPublicKey {
	if p == nil {
		return nil
	}
	return &services.ECDHPublicKey{
		PublicKey: p.PublicKey,
		Curve:     p.Curve,
	}
}

func ecdhPublicKeyToProto(k *services.ECDHPublicKey) *common.ECDHPublicKey {
	if k == nil {
		return nil
	}
	return &common.ECDHPublicKey{
		PublicKey: k.PublicKey,
		Curve:     k.Curve,
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

func ecdhEncryptedDEKToProto(d *services.ECDHEncryptedDEK) *common.ECDHEncryptedDEK {
	if d == nil {
		return nil
	}
	return &common.ECDHEncryptedDEK{
		Ciphertext:         d.Ciphertext,
		Nonce:              d.Nonce,
		EphemeralPublicKey: d.EphemeralPublicKey,
	}
}

var _ server.ServiceServer = (*GRPCServer)(nil)
