package keyunwrap_service

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"cipherlake/internal/services"
	"cipherlake/internal/services/server"
	common "cipherlake/proto/common"
	pb "cipherlake/proto/keyunwrap"
)

// GRPCServer adapts KeyUnwrapService to gRPC interface
type GRPCServer struct {
	pb.UnimplementedKeyUnwrapServiceServer
	service *KeyUnwrapService
}

// NewGRPCServer creates a new gRPC server adapter
func NewGRPCServer(service *KeyUnwrapService) *GRPCServer {
	return &GRPCServer{service: service}
}

// Register registers the service with a gRPC server
func (g *GRPCServer) Register(grpcServer *grpc.Server) error {
	pb.RegisterKeyUnwrapServiceServer(grpcServer, g)
	return nil
}

// Close closes the underlying service
func (g *GRPCServer) Close() error {
	return g.service.Close()
}

// UnwrapKey handles gRPC request
func (g *GRPCServer) UnwrapKey(ctx context.Context, req *pb.UnwrapKeyRequest) (*pb.UnwrapKeyResponse, error) {
	token := req.GetToken()
	if token == nil {
		return &pb.UnwrapKeyResponse{
			Error: &common.Error{Code: 400, Message: "token is required"},
		}, nil
	}

	encryptedDEK := protoToEncryptedDEK(req.GetEncryptedDek())
	clientECDHPub := protoToECDHPublicKey(req.GetClientEcdhPub())

	ecdhEncryptedDEK, serviceECDHPub, err := g.service.UnwrapKey(
		ctx,
		token.GetTokenId(),
		token.GetUserId(),
		token.GetBucket(),
		token.GetObjectKey(),
		encryptedDEK,
		clientECDHPub,
	)
	if err != nil {
		zap.L().Error("failed to unwrap key", zap.Error(err))
		return &pb.UnwrapKeyResponse{
			Error: &common.Error{Code: 500, Message: err.Error()},
		}, nil
	}

	return &pb.UnwrapKeyResponse{
		EcdhEncryptedDek: ecdhEncryptedDEKToProto(ecdhEncryptedDEK),
		ServiceEcdhPub:   ecdhPublicKeyToProto(serviceECDHPub),
	}, nil
}

// Health handles gRPC health check
func (g *GRPCServer) Health(ctx context.Context, req *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{
		Healthy:     true,
		ServiceName: "keyunwrap-service",
		Version:     "1.0.0",
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
