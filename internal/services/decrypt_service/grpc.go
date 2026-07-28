package decrypt_service

import (
	"context"
	"crypto/ecdh"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"cipherlake/internal/services"
	"cipherlake/internal/services/server"
	common "cipherlake/proto/common"
	pb "cipherlake/proto/decrypt"
)

// GRPCServer adapts DecryptService to gRPC interface
type GRPCServer struct {
	pb.UnimplementedDecryptServiceServer
	service *DecryptService
}

// NewGRPCServer creates a new gRPC server adapter
func NewGRPCServer(service *DecryptService) *GRPCServer {
	return &GRPCServer{service: service}
}

// Register registers the service with a gRPC server
func (g *GRPCServer) Register(grpcServer *grpc.Server) error {
	pb.RegisterDecryptServiceServer(grpcServer, g)
	return nil
}

// Close closes the underlying service
func (g *GRPCServer) Close() error {
	return g.service.Close()
}

// Decrypt handles gRPC server streaming request
func (g *GRPCServer) Decrypt(req *pb.DecryptRequest, stream grpc.ServerStreamingServer[pb.DecryptResponse]) error {
	clientECDHPriv, err := ecdh.P256().NewPrivateKey(req.GetClientEcdhPrivateKey())
	if err != nil {
		zap.L().Error("failed to parse client ECDH private key", zap.Error(err))
		return stream.Send(&pb.DecryptResponse{
			Error: &common.Error{Code: 400, Message: "invalid client ECDH private key"},
		})
	}

	serviceECDHPub := protoToECDHPublicKey(req.GetServiceEcdhPub())
	ecdhEncryptedDEK := protoToECDHEncryptedDEK(req.GetEcdhEncryptedDek())

	// For streaming Decrypt, we don't have ciphertext in the request,
	// so send a response indicating the stream is ready (no data to decrypt in this model)
	// The actual decryption is done via DecryptChunk
	_ = clientECDHPriv
	_ = serviceECDHPub
	_ = ecdhEncryptedDEK

	return stream.Send(&pb.DecryptResponse{
		Sequence: 0,
		IsLast:   true,
	})
}

// DecryptChunk handles gRPC request for single chunk decryption
func (g *GRPCServer) DecryptChunk(ctx context.Context, req *pb.DecryptChunkRequest) (*pb.DecryptChunkResponse, error) {
	clientECDHPriv, err := ecdh.P256().NewPrivateKey(req.GetClientEcdhPrivateKey())
	if err != nil {
		zap.L().Error("failed to parse client ECDH private key", zap.Error(err))
		return &pb.DecryptChunkResponse{
			Error: &common.Error{Code: 400, Message: "invalid client ECDH private key"},
		}, nil
	}

	serviceECDHPub := protoToECDHPublicKey(req.GetServiceEcdhPub())
	ecdhEncryptedDEK := protoToECDHEncryptedDEK(req.GetEcdhEncryptedDek())

	plaintext, err := g.service.Decrypt(
		clientECDHPriv,
		serviceECDHPub,
		ecdhEncryptedDEK,
		req.GetCiphertext(),
		req.GetNonce(),
		req.GetAuthTag(),
		req.GetAlgorithm(),
	)
	if err != nil {
		zap.L().Error("failed to decrypt chunk", zap.Error(err))
		return &pb.DecryptChunkResponse{
			Error: &common.Error{Code: 500, Message: err.Error()},
		}, nil
	}

	return &pb.DecryptChunkResponse{
		Plaintext: plaintext,
	}, nil
}

// Health handles gRPC health check
func (g *GRPCServer) Health(ctx context.Context, req *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{
		Healthy:     true,
		ServiceName: "decrypt-service",
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
