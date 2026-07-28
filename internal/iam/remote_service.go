package iam

import (
	"context"
	"fmt"
	"time"

	pb "cipherlake/proto/sts"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RemoteIAMService implements IAMServiceProvider via gRPC calls to sts-service.
// This allows cipherlake to perform IAM lookups without opening the BoltDB database
// directly, which is required in distributed mode where sts-service owns the DB.
type RemoteIAMService struct {
	conn   *grpc.ClientConn
	client pb.STSServiceClient
}

// NewRemoteIAMService creates a new remote IAM service that connects to sts-service
func NewRemoteIAMService(addr string) (*RemoteIAMService, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect sts-service at %s: %w", addr, err)
	}

	return &RemoteIAMService{
		conn:   conn,
		client: pb.NewSTSServiceClient(conn),
	}, nil
}

// Close closes the gRPC connection
func (r *RemoteIAMService) Close() error {
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// Client returns the gRPC client for use by RemoteAdminAPI
func (r *RemoteIAMService) Client() pb.STSServiceClient {
	return r.client
}

// GetUserByAccessKeyID looks up a user by access key ID via gRPC
func (r *RemoteIAMService) GetUserByAccessKeyID(accessKeyID string) (*IAMUser, *AccessKey, error) {
	resp, err := r.client.LookupUserByAccessKey(context.Background(), &pb.LookupUserByAccessKeyRequest{
		AccessKeyId: accessKeyID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("rpc LookupUserByAccessKey: %w", err)
	}

	user := protoToIAMUser(resp.User)
	ak := protoToAccessKey(resp.AccessKey)
	return user, ak, nil
}

// DecryptSecretKey decrypts an encrypted secret key via gRPC
func (r *RemoteIAMService) DecryptSecretKey(encryptedSecret []byte) (string, error) {
	resp, err := r.client.DecryptSecretKey(context.Background(), &pb.DecryptSecretKeyRequest{
		EncryptedSecret: encryptedSecret,
	})
	if err != nil {
		return "", fmt.Errorf("rpc DecryptSecretKey: %w", err)
	}
	return resp.DecryptedSecret, nil
}

// GetTempCredentialByAccessKeyID retrieves a temp credential by access key ID via gRPC
func (r *RemoteIAMService) GetTempCredentialByAccessKeyID(accessKeyID string) (*TemporaryCredential, error) {
	resp, err := r.client.LookupTempCredential(context.Background(), &pb.LookupTempCredentialRequest{
		AccessKeyId: accessKeyID,
	})
	if err != nil {
		return nil, fmt.Errorf("rpc LookupTempCredential: %w", err)
	}

	return &TemporaryCredential{
		AccessKeyID:     resp.AccessKeyId,
		SecretAccessKey: resp.SecretAccessKey,
		SessionToken:    resp.SessionToken,
		Expiration:      time.Unix(resp.ExpirationUnix, 0),
	}, nil
}

// GetUser gets a user by name via gRPC
func (r *RemoteIAMService) GetUser(name string) (*IAMUser, error) {
	resp, err := r.client.LookupUser(context.Background(), &pb.LookupUserRequest{
		UserName: name,
	})
	if err != nil {
		return nil, fmt.Errorf("rpc LookupUser: %w", err)
	}
	return protoToIAMUser(resp.User), nil
}

// EvaluateAccess evaluates IAM policy for a request via gRPC
func (r *RemoteIAMService) EvaluateAccess(ctx *EvalContext) *EvalResult {
	resp, err := r.client.EvaluateAccess(context.Background(), &pb.EvaluateAccessRequest{
		Principal:       ctx.Principal,
		Action:          ctx.Action,
		Resource:        ctx.Resource,
		SourceIp:        ctx.SourceIP,
		RequestTimeUnix: ctx.Time.Unix(),
		Conditions:      ctx.Conditions,
	})
	if err != nil {
		return &EvalResult{
			Decision: DecisionImplicitDeny,
			Details:  fmt.Sprintf("rpc EvaluateAccess failed: %v", err),
		}
	}

	return &EvalResult{
		Decision:   decisionFromString(resp.Decision),
		MatchedBy:  resp.MatchedBy,
		PolicyType: resp.PolicyType,
		PolicyName: resp.PolicyName,
		Details:    resp.Details,
	}
}

// --- Proto conversion helpers ---

func protoToIAMUser(pbUser *pb.IAMUserProto) *IAMUser {
	if pbUser == nil {
		return nil
	}
	user := &IAMUser{
		ID:                 pbUser.Id,
		Name:               pbUser.Name,
		DisplayName:        pbUser.DisplayName,
		Groups:             pbUser.Groups,
		AttachedPolicies:   pbUser.AttachedPolicies,
		PermissionBoundary: pbUser.PermissionBoundary,
		CreatedAt:          time.Unix(pbUser.CreatedAtUnix, 0),
	}
	for _, pbAK := range pbUser.AccessKeys {
		user.AccessKeys = append(user.AccessKeys, *protoToAccessKey(pbAK))
	}
	return user
}

func protoToAccessKey(pbAK *pb.AccessKeyProto) *AccessKey {
	if pbAK == nil {
		return nil
	}
	return &AccessKey{
		AccessKeyID:  pbAK.AccessKeyId,
		SecretKeyEnc: pbAK.SecretKeyEnc,
		Status:       pbAK.Status,
		CreatedAt:    time.Unix(pbAK.CreatedAtUnix, 0),
		Description:  pbAK.Description,
	}
}

func decisionFromString(s string) Decision {
	switch s {
	case "Allow":
		return DecisionAllow
	case "Deny":
		return DecisionDeny
	default:
		return DecisionImplicitDeny
	}
}

// Compile-time check that RemoteIAMService implements IAMServiceProvider
var _ IAMServiceProvider = (*RemoteIAMService)(nil)
