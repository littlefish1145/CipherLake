package token_service

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	common "cipherlake/proto/common"
	pb "cipherlake/proto/token"
)

// GRPCServer adapts TokenService to gRPC interface
type GRPCServer struct {
	pb.UnimplementedTokenServiceServer
	service *TokenService
}

// NewGRPCServer creates a new gRPC server adapter
func NewGRPCServer(service *TokenService) *GRPCServer {
	return &GRPCServer{service: service}
}

// Register registers the service with a gRPC server
func (g *GRPCServer) Register(grpcServer *grpc.Server) error {
	pb.RegisterTokenServiceServer(grpcServer, g)
	return nil
}

// Close closes the underlying service
func (g *GRPCServer) Close() error {
	return g.service.Close()
}

// IssueWriteToken handles gRPC request
func (g *GRPCServer) IssueWriteToken(ctx context.Context, req *pb.IssueWriteTokenRequest) (*pb.IssueWriteTokenResponse, error) {
	token, err := g.service.IssueWriteToken(ctx, req.UserId, req.Bucket, req.ObjectKey, req.TtlSeconds)
	if err != nil {
		zap.L().Error("failed to issue write token", zap.Error(err))
		return &pb.IssueWriteTokenResponse{Error: &common.Error{Code: 500, Message: err.Error()}}, nil
	}
	return &pb.IssueWriteTokenResponse{Token: tokenToProto(token)}, nil
}

// IssueReadToken handles gRPC request
func (g *GRPCServer) IssueReadToken(ctx context.Context, req *pb.IssueReadTokenRequest) (*pb.IssueReadTokenResponse, error) {
	token, err := g.service.IssueReadToken(ctx, req.UserId, req.Bucket, req.ObjectKey, req.ContentHash, req.TtlSeconds)
	if err != nil {
		zap.L().Error("failed to issue read token", zap.Error(err))
		return &pb.IssueReadTokenResponse{Error: &common.Error{Code: 500, Message: err.Error()}}, nil
	}
	return &pb.IssueReadTokenResponse{Token: tokenToProto(token)}, nil
}

// IssueDeleteToken handles gRPC request
func (g *GRPCServer) IssueDeleteToken(ctx context.Context, req *pb.IssueDeleteTokenRequest) (*pb.IssueDeleteTokenResponse, error) {
	token, err := g.service.IssueDeleteToken(ctx, req.UserId, req.Bucket, req.ObjectKey, req.TtlSeconds)
	if err != nil {
		zap.L().Error("failed to issue delete token", zap.Error(err))
		return &pb.IssueDeleteTokenResponse{Error: &common.Error{Code: 500, Message: err.Error()}}, nil
	}
	return &pb.IssueDeleteTokenResponse{Token: tokenToProto(token)}, nil
}

// ValidateToken handles gRPC request
func (g *GRPCServer) ValidateToken(ctx context.Context, req *pb.ValidateTokenRequest) (*pb.ValidateTokenResponse, error) {
	token := protoToToken(req.Token)
	expectedType := TokenType(req.ExpectedType)
	err := g.service.ValidateToken(ctx, token, expectedType)
	if err != nil {
		return &pb.ValidateTokenResponse{
			Valid: false,
			Error: &common.Error{Code: 401, Message: err.Error()},
		}, nil
	}
	return &pb.ValidateTokenResponse{
		Valid:     true,
		UserId:    token.UserID,
		Bucket:    token.Bucket,
		ObjectKey: token.ObjectKey,
	}, nil
}

// GetPublicKey handles gRPC request
func (g *GRPCServer) GetPublicKey(ctx context.Context, req *pb.GetPublicKeyRequest) (*pb.GetPublicKeyResponse, error) {
	pubKey, keyID := g.service.GetPublicKey()
	return &pb.GetPublicKeyResponse{
		PublicKey: pubKey,
		KeyId:     keyID,
	}, nil
}

// Health handles gRPC health check
func (g *GRPCServer) Health(ctx context.Context, req *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{
		Healthy:     true,
		ServiceName: "token-service",
		Version:     "1.0.0",
	}, nil
}

func tokenToProto(token *DelegationToken) *common.DelegationToken {
	return &common.DelegationToken{
		TokenId:     token.TokenID,
		TokenType:   common.TokenType(token.TokenType),
		UserId:      token.UserID,
		Bucket:      token.Bucket,
		ObjectKey:   token.ObjectKey,
		Expiry:      token.Expiry.Unix(),
		CreatedAt:   token.CreatedAt.Unix(),
		Operations:  token.Operations,
		ContentHash: token.ContentHash,
		Signature:   token.Signature,
	}
}

func protoToToken(p *common.DelegationToken) *DelegationToken {
	if p == nil {
		return nil
	}
	return &DelegationToken{
		TokenID:     p.TokenId,
		TokenType:   TokenType(p.TokenType),
		UserID:      p.UserId,
		Bucket:      p.Bucket,
		ObjectKey:   p.ObjectKey,
		Expiry:      time.Unix(p.Expiry, 0),
		CreatedAt:   time.Unix(p.CreatedAt, 0),
		Operations:  p.Operations,
		ContentHash: p.ContentHash,
		Signature:   p.Signature,
	}
}
