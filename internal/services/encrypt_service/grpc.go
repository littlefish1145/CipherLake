package encrypt_service

import (
	"context"
	"crypto/ecdh"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"cipherlake/internal/services"
	"cipherlake/internal/services/server"
	common "cipherlake/proto/common"
	pb "cipherlake/proto/encrypt"
)

// GRPCServer adapts EncryptService to gRPC interface
type GRPCServer struct {
	pb.UnimplementedEncryptServiceServer
	service *EncryptService
}

// NewGRPCServer creates a new gRPC server adapter
func NewGRPCServer(service *EncryptService) *GRPCServer {
	return &GRPCServer{service: service}
}

// Register registers the service with a gRPC server
func (g *GRPCServer) Register(grpcServer *grpc.Server) error {
	pb.RegisterEncryptServiceServer(grpcServer, g)
	return nil
}

// Close closes the underlying service
func (g *GRPCServer) Close() error {
	return g.service.Close()
}

// Encrypt handles gRPC server streaming request
func (g *GRPCServer) Encrypt(req *pb.EncryptRequest, stream grpc.ServerStreamingServer[pb.EncryptResponse]) error {
	clientECDHPriv, err := ecdh.P256().NewPrivateKey(req.GetClientEcdhPrivateKey())
	if err != nil {
		zap.L().Error("failed to parse client ECDH private key", zap.Error(err))
		return stream.Send(&pb.EncryptResponse{
			Error: &common.Error{Code: 400, Message: "invalid client ECDH private key"},
		})
	}

	serviceECDHPub := protoToECDHPublicKey(req.GetServiceEcdhPub())
	ecdhEncryptedDEK := protoToECDHEncryptedDEK(req.GetEcdhEncryptedDek())

	// For streaming Encrypt, we don't have plaintext in the request,
	// so send a response indicating the stream is ready (no data to encrypt in this model)
	// The actual encryption is done via EncryptChunk
	_ = clientECDHPriv
	_ = serviceECDHPub
	_ = ecdhEncryptedDEK

	return stream.Send(&pb.EncryptResponse{
		Sequence: 0,
		IsLast:   true,
	})
}

// EncryptChunk handles gRPC request for single chunk encryption
func (g *GRPCServer) EncryptChunk(ctx context.Context, req *pb.EncryptChunkRequest) (*pb.EncryptChunkResponse, error) {
	clientECDHPriv, err := ecdh.P256().NewPrivateKey(req.GetClientEcdhPrivateKey())
	if err != nil {
		zap.L().Error("failed to parse client ECDH private key", zap.Error(err))
		return &pb.EncryptChunkResponse{
			Error: &common.Error{Code: 400, Message: "invalid client ECDH private key"},
		}, nil
	}

	serviceECDHPub := protoToECDHPublicKey(req.GetServiceEcdhPub())
	ecdhEncryptedDEK := protoToECDHEncryptedDEK(req.GetEcdhEncryptedDek())

	ciphertext, nonce, authTag, err := g.service.Encrypt(
		clientECDHPriv,
		serviceECDHPub,
		ecdhEncryptedDEK,
		req.GetPlaintext(),
		req.GetAlgorithm(),
	)
	if err != nil {
		zap.L().Error("failed to encrypt chunk", zap.Error(err))
		return &pb.EncryptChunkResponse{
			Error: &common.Error{Code: 500, Message: err.Error()},
		}, nil
	}

	return &pb.EncryptChunkResponse{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		AuthTag:    authTag,
	}, nil
}

// Health handles gRPC health check
func (g *GRPCServer) Health(ctx context.Context, req *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{
		Healthy:     true,
		ServiceName: "encrypt-service",
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

func protoToECDHEncryptedDEK(p *common.ECDHEncryptedDEK) *services.ECDHEncryptedDEK {
	if p == nil {
		return nil
	}
	return &services.ECDHEncryptedDEK{
		Ciphertext:         p.Ciphertext,
		Nonce:              p.Nonce,
		EphemeralPublicKey: p.EphemeralPublicKey,
	}
}

var _ server.ServiceServer = (*GRPCServer)(nil)
