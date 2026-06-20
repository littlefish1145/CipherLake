package sts_service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"nexus/internal/iam"

	pb "nexus/proto/sts"

	"go.uber.org/zap"
)

// GRPCAdapter adapts the STS service to gRPC interface
type GRPCAdapter struct {
	pb.UnimplementedSTSServiceServer
	service *STSService
}

// NewGRPCAdapter creates a new gRPC adapter for STS
func NewGRPCAdapter(service *STSService) *GRPCAdapter {
	return &GRPCAdapter{service: service}
}

// AssumeRole handles gRPC AssumeRole requests
func (a *GRPCAdapter) AssumeRole(ctx context.Context, req *pb.AssumeRoleRequest) (*pb.AssumeRoleResponse, error) {
	zap.L().Info("gRPC AssumeRole",
		zap.String("role_arn", req.RoleArn),
		zap.String("caller_arn", req.CallerArn))

	cred, err := a.service.AssumeRole(ctx, req.RoleArn, req.RoleSessionName, int(req.DurationSeconds), req.ExternalId, req.Policy, req.CallerArn)
	if err != nil {
		return nil, err
	}

	return &pb.AssumeRoleResponse{
		Credentials: &pb.Credentials{
			AccessKeyId:     cred.AccessKeyID,
			SecretAccessKey: cred.SecretAccessKey,
			SessionToken:    cred.SessionToken,
			ExpirationUnix:  cred.Expiration.Unix(),
		},
		AssumedRoleUserArn: req.RoleArn,
	}, nil
}

// GetSessionToken handles gRPC GetSessionToken requests
func (a *GRPCAdapter) GetSessionToken(ctx context.Context, req *pb.GetSessionTokenRequest) (*pb.GetSessionTokenResponse, error) {
	zap.L().Info("gRPC GetSessionToken", zap.String("caller_arn", req.CallerArn))

	cred, err := a.service.GetSessionToken(ctx, req.CallerArn, int(req.DurationSeconds))
	if err != nil {
		return nil, err
	}

	return &pb.GetSessionTokenResponse{
		Credentials: &pb.Credentials{
			AccessKeyId:     cred.AccessKeyID,
			SecretAccessKey: cred.SecretAccessKey,
			SessionToken:    cred.SessionToken,
			ExpirationUnix:  cred.Expiration.Unix(),
		},
	}, nil
}

// GetFederationToken handles gRPC GetFederationToken requests
func (a *GRPCAdapter) GetFederationToken(ctx context.Context, req *pb.GetFederationTokenRequest) (*pb.GetFederationTokenResponse, error) {
	zap.L().Info("gRPC GetFederationToken", zap.String("name", req.Name))

	cred, err := a.service.GetFederationToken(ctx, req.Name, req.CallerArn, int(req.DurationSeconds), req.Policy)
	if err != nil {
		return nil, err
	}

	return &pb.GetFederationTokenResponse{
		Credentials: &pb.Credentials{
			AccessKeyId:     cred.AccessKeyID,
			SecretAccessKey: cred.SecretAccessKey,
			SessionToken:    cred.SessionToken,
			ExpirationUnix:  cred.Expiration.Unix(),
		},
	}, nil
}

// --- IAM Lookup RPCs ---

// LookupUserByAccessKey handles gRPC LookupUserByAccessKey requests
func (a *GRPCAdapter) LookupUserByAccessKey(ctx context.Context, req *pb.LookupUserByAccessKeyRequest) (*pb.LookupUserByAccessKeyResponse, error) {
	user, ak, err := a.service.iamService.GetUserByAccessKeyID(req.AccessKeyId)
	if err != nil {
		return nil, err
	}

	return &pb.LookupUserByAccessKeyResponse{
		User:      iamUserToProto(user),
		AccessKey: accessKeyToProto(ak),
	}, nil
}

// DecryptSecretKey handles gRPC DecryptSecretKey requests
func (a *GRPCAdapter) DecryptSecretKey(ctx context.Context, req *pb.DecryptSecretKeyRequest) (*pb.DecryptSecretKeyResponse, error) {
	decrypted, err := a.service.iamService.DecryptSecretKey(req.EncryptedSecret)
	if err != nil {
		return nil, err
	}

	return &pb.DecryptSecretKeyResponse{
		DecryptedSecret: decrypted,
	}, nil
}

// LookupTempCredential handles gRPC LookupTempCredential requests
func (a *GRPCAdapter) LookupTempCredential(ctx context.Context, req *pb.LookupTempCredentialRequest) (*pb.LookupTempCredentialResponse, error) {
	cred, err := a.service.iamService.GetTempCredentialByAccessKeyID(req.AccessKeyId)
	if err != nil {
		return nil, err
	}

	return &pb.LookupTempCredentialResponse{
		AccessKeyId:     cred.AccessKeyID,
		SecretAccessKey: cred.SecretAccessKey,
		SessionToken:    cred.SessionToken,
		ExpirationUnix:  cred.Expiration.Unix(),
	}, nil
}

// LookupUser handles gRPC LookupUser requests
func (a *GRPCAdapter) LookupUser(ctx context.Context, req *pb.LookupUserRequest) (*pb.LookupUserResponse, error) {
	user, err := a.service.iamService.GetUser(req.UserName)
	if err != nil {
		return nil, err
	}

	return &pb.LookupUserResponse{
		User: iamUserToProto(user),
	}, nil
}

// EvaluateAccess handles gRPC EvaluateAccess requests
func (a *GRPCAdapter) EvaluateAccess(ctx context.Context, req *pb.EvaluateAccessRequest) (*pb.EvaluateAccessResponse, error) {
	evalCtx := &iam.EvalContext{
		Principal:  req.Principal,
		Action:     req.Action,
		Resource:   req.Resource,
		SourceIP:   req.SourceIp,
		Conditions: req.Conditions,
	}
	if req.RequestTimeUnix > 0 {
		evalCtx.Time = time.Unix(req.RequestTimeUnix, 0)
	} else {
		evalCtx.Time = time.Now()
	}

	result := a.service.iamService.EvaluateAccess(evalCtx)

	return &pb.EvaluateAccessResponse{
		Decision:   result.Decision.String(),
		MatchedBy:  result.MatchedBy,
		PolicyType: result.PolicyType,
		PolicyName: result.PolicyName,
		Details:    result.Details,
	}, nil
}

// --- Proto conversion helpers ---

func iamUserToProto(user *iam.IAMUser) *pb.IAMUserProto {
	if user == nil {
		return nil
	}
	pbUser := &pb.IAMUserProto{
		Id:                user.ID,
		Name:              user.Name,
		DisplayName:       user.DisplayName,
		Groups:            user.Groups,
		AttachedPolicies:  user.AttachedPolicies,
		PermissionBoundary: user.PermissionBoundary,
		CreatedAtUnix:     user.CreatedAt.Unix(),
	}
	for _, ak := range user.AccessKeys {
		pbUser.AccessKeys = append(pbUser.AccessKeys, accessKeyToProto(&ak))
	}
	return pbUser
}

func accessKeyToProto(ak *iam.AccessKey) *pb.AccessKeyProto {
	if ak == nil {
		return nil
	}
	return &pb.AccessKeyProto{
		AccessKeyId:   ak.AccessKeyID,
		SecretKeyEnc:  ak.SecretKeyEnc,
		Status:        ak.Status,
		CreatedAtUnix: ak.CreatedAt.Unix(),
		Description:   ak.Description,
	}
}

// --- IAM Admin RPCs ---

func (a *GRPCAdapter) AdminCreateUser(ctx context.Context, req *pb.AdminCreateUserRequest) (*pb.AdminCreateUserResponse, error) {
	user, err := a.service.iamService.CreateUser(req.Name, req.DisplayName)
	if err != nil {
		return nil, err
	}
	return &pb.AdminCreateUserResponse{User: iamUserToProto(user)}, nil
}

func (a *GRPCAdapter) AdminDeleteUser(ctx context.Context, req *pb.AdminDeleteUserRequest) (*pb.AdminDeleteUserResponse, error) {
	if err := a.service.iamService.DeleteUser(req.UserName); err != nil {
		return nil, err
	}
	return &pb.AdminDeleteUserResponse{}, nil
}

func (a *GRPCAdapter) AdminListUsers(ctx context.Context, req *pb.AdminListUsersRequest) (*pb.AdminListUsersResponse, error) {
	users, err := a.service.iamService.ListUsers()
	if err != nil {
		return nil, err
	}
	var pbUsers []*pb.IAMUserProto
	for _, u := range users {
		pbUsers = append(pbUsers, iamUserToProto(u))
	}
	return &pb.AdminListUsersResponse{Users: pbUsers}, nil
}

func (a *GRPCAdapter) AdminCreateAccessKey(ctx context.Context, req *pb.AdminCreateAccessKeyRequest) (*pb.AdminCreateAccessKeyResponse, error) {
	result, err := a.service.iamService.CreateAccessKey(req.UserName, req.Description)
	if err != nil {
		return nil, err
	}
	return &pb.AdminCreateAccessKeyResponse{
		AccessKeyId:     result.AccessKeyID,
		SecretAccessKey: result.SecretAccessKey,
		Status:          result.Status,
		CreatedAtUnix:   result.CreatedAt.Unix(),
	}, nil
}

func (a *GRPCAdapter) AdminListAccessKeys(ctx context.Context, req *pb.AdminListAccessKeysRequest) (*pb.AdminListAccessKeysResponse, error) {
	keys, err := a.service.iamService.ListAccessKeys(req.UserName)
	if err != nil {
		return nil, err
	}
	var pbKeys []*pb.AccessKeyProto
	for i := range keys {
		pbKeys = append(pbKeys, accessKeyToProto(&keys[i]))
	}
	return &pb.AdminListAccessKeysResponse{AccessKeys: pbKeys}, nil
}

func (a *GRPCAdapter) AdminDeleteAccessKey(ctx context.Context, req *pb.AdminDeleteAccessKeyRequest) (*pb.AdminDeleteAccessKeyResponse, error) {
	if err := a.service.iamService.DeleteAccessKey(req.UserName, req.AccessKeyId); err != nil {
		return nil, err
	}
	return &pb.AdminDeleteAccessKeyResponse{}, nil
}

func (a *GRPCAdapter) AdminListGroups(ctx context.Context, req *pb.AdminListGroupsRequest) (*pb.AdminListGroupsResponse, error) {
	groups, err := a.service.iamService.ListGroups()
	if err != nil {
		return nil, err
	}
	var pbGroups []*pb.AdminListGroupResponse
	for _, g := range groups {
		pbGroups = append(pbGroups, &pb.AdminListGroupResponse{
			Name:             g.Name,
			Description:      g.Description,
			Users:            g.Users,
			AttachedPolicies: g.AttachedPolicies,
			CreatedAtUnix:    g.CreatedAt.Unix(),
		})
	}
	return &pb.AdminListGroupsResponse{Groups: pbGroups}, nil
}

func (a *GRPCAdapter) AdminCreateGroup(ctx context.Context, req *pb.AdminCreateGroupRequest) (*pb.AdminCreateGroupResponse, error) {
	group, err := a.service.iamService.CreateGroup(req.Name, req.Description)
	if err != nil {
		return nil, err
	}
	return &pb.AdminCreateGroupResponse{
		Name:          group.Name,
		Description:   group.Description,
		CreatedAtUnix: group.CreatedAt.Unix(),
	}, nil
}

func (a *GRPCAdapter) AdminDeleteGroup(ctx context.Context, req *pb.AdminDeleteGroupRequest) (*pb.AdminDeleteGroupResponse, error) {
	if err := a.service.iamService.DeleteGroup(req.GroupName); err != nil {
		return nil, err
	}
	return &pb.AdminDeleteGroupResponse{}, nil
}

func (a *GRPCAdapter) AdminAddUserToGroup(ctx context.Context, req *pb.AdminAddUserToGroupRequest) (*pb.AdminAddUserToGroupResponse, error) {
	if err := a.service.iamService.AddUserToGroup(req.UserName, req.GroupName); err != nil {
		return nil, err
	}
	return &pb.AdminAddUserToGroupResponse{}, nil
}

func (a *GRPCAdapter) AdminRemoveUserFromGroup(ctx context.Context, req *pb.AdminRemoveUserFromGroupRequest) (*pb.AdminRemoveUserFromGroupResponse, error) {
	if err := a.service.iamService.RemoveUserFromGroup(req.UserName, req.GroupName); err != nil {
		return nil, err
	}
	return &pb.AdminRemoveUserFromGroupResponse{}, nil
}

func (a *GRPCAdapter) AdminListPolicies(ctx context.Context, req *pb.AdminListPoliciesRequest) (*pb.AdminListPoliciesResponse, error) {
	policies, err := a.service.iamService.ListPolicies()
	if err != nil {
		return nil, err
	}
	var pbPolicies []*pb.AdminListPolicyItem
	for _, p := range policies {
		pbPolicies = append(pbPolicies, &pb.AdminListPolicyItem{
			Arn:           p.ARN,
			Name:          p.Name,
			Description:   p.Description,
			Type:          p.Type,
			CreatedAtUnix: p.CreatedAt.Unix(),
			UpdatedAtUnix: p.UpdatedAt.Unix(),
		})
	}
	return &pb.AdminListPoliciesResponse{Policies: pbPolicies}, nil
}

func (a *GRPCAdapter) AdminCreatePolicy(ctx context.Context, req *pb.AdminCreatePolicyRequest) (*pb.AdminCreatePolicyResponse, error) {
	doc, err := iam.ParsePolicyDocumentFromString(req.DocumentJson)
	if err != nil {
		return nil, fmt.Errorf("invalid policy document: %w", err)
	}
	policy, err := a.service.iamService.CreatePolicy(req.Name, req.Description, *doc)
	if err != nil {
		return nil, err
	}
	return &pb.AdminCreatePolicyResponse{Arn: policy.ARN, Name: policy.Name}, nil
}

func (a *GRPCAdapter) AdminDeletePolicy(ctx context.Context, req *pb.AdminDeletePolicyRequest) (*pb.AdminDeletePolicyResponse, error) {
	if err := a.service.iamService.DeletePolicy(req.PolicyName); err != nil {
		return nil, err
	}
	return &pb.AdminDeletePolicyResponse{}, nil
}

func (a *GRPCAdapter) AdminAttachUserPolicy(ctx context.Context, req *pb.AdminAttachUserPolicyRequest) (*pb.AdminAttachUserPolicyResponse, error) {
	if err := a.service.iamService.AttachUserPolicy(req.UserName, req.PolicyName); err != nil {
		return nil, err
	}
	return &pb.AdminAttachUserPolicyResponse{}, nil
}

func (a *GRPCAdapter) AdminDetachUserPolicy(ctx context.Context, req *pb.AdminDetachUserPolicyRequest) (*pb.AdminDetachUserPolicyResponse, error) {
	if err := a.service.iamService.DetachUserPolicy(req.UserName, req.PolicyName); err != nil {
		return nil, err
	}
	return &pb.AdminDetachUserPolicyResponse{}, nil
}

func (a *GRPCAdapter) AdminAttachGroupPolicy(ctx context.Context, req *pb.AdminAttachGroupPolicyRequest) (*pb.AdminAttachGroupPolicyResponse, error) {
	if err := a.service.iamService.AttachGroupPolicy(req.GroupName, req.PolicyName); err != nil {
		return nil, err
	}
	return &pb.AdminAttachGroupPolicyResponse{}, nil
}

func (a *GRPCAdapter) AdminListRoles(ctx context.Context, req *pb.AdminListRolesRequest) (*pb.AdminListRolesResponse, error) {
	roles, err := a.service.iamService.ListRoles()
	if err != nil {
		return nil, err
	}
	var pbRoles []*pb.AdminListRoleItem
	for _, r := range roles {
		pbRoles = append(pbRoles, &pb.AdminListRoleItem{
			Name:               r.Name,
			Description:        r.Description,
			PermissionPolicies: r.PermissionPolicies,
			CreatedAtUnix:      r.CreatedAt.Unix(),
		})
	}
	return &pb.AdminListRolesResponse{Roles: pbRoles}, nil
}

func (a *GRPCAdapter) AdminCreateRole(ctx context.Context, req *pb.AdminCreateRoleRequest) (*pb.AdminCreateRoleResponse, error) {
	trustDoc, err := iam.ParsePolicyDocumentFromString(req.TrustPolicyJson)
	if err != nil {
		return nil, fmt.Errorf("invalid trust policy: %w", err)
	}
	role, err := a.service.iamService.CreateRole(req.Name, req.Description, *trustDoc, int(req.MaxSessionDuration))
	if err != nil {
		return nil, err
	}
	return &pb.AdminCreateRoleResponse{Name: role.Name}, nil
}

func (a *GRPCAdapter) AdminDeleteRole(ctx context.Context, req *pb.AdminDeleteRoleRequest) (*pb.AdminDeleteRoleResponse, error) {
	if err := a.service.iamService.DeleteRole(req.RoleName); err != nil {
		return nil, err
	}
	return &pb.AdminDeleteRoleResponse{}, nil
}

func (a *GRPCAdapter) AdminAttachRolePolicy(ctx context.Context, req *pb.AdminAttachRolePolicyRequest) (*pb.AdminAttachRolePolicyResponse, error) {
	if err := a.service.iamService.AttachRolePolicy(req.RoleName, req.PolicyName); err != nil {
		return nil, err
	}
	return &pb.AdminAttachRolePolicyResponse{}, nil
}

func (a *GRPCAdapter) AdminGetBucketPolicy(ctx context.Context, req *pb.AdminGetBucketPolicyRequest) (*pb.AdminGetBucketPolicyResponse, error) {
	bp, err := a.service.iamService.GetBucketPolicy(req.Bucket)
	if err != nil {
		return nil, err
	}
	docJSON, _ := json.Marshal(bp.Document)
	return &pb.AdminGetBucketPolicyResponse{DocumentJson: string(docJSON)}, nil
}

func (a *GRPCAdapter) AdminPutBucketPolicy(ctx context.Context, req *pb.AdminPutBucketPolicyRequest) (*pb.AdminPutBucketPolicyResponse, error) {
	doc, err := iam.ParsePolicyDocumentFromString(req.DocumentJson)
	if err != nil {
		return nil, fmt.Errorf("invalid policy document: %w", err)
	}
	if err := a.service.iamService.PutBucketPolicy(req.Bucket, *doc); err != nil {
		return nil, err
	}
	return &pb.AdminPutBucketPolicyResponse{}, nil
}

func (a *GRPCAdapter) AdminDeleteBucketPolicy(ctx context.Context, req *pb.AdminDeleteBucketPolicyRequest) (*pb.AdminDeleteBucketPolicyResponse, error) {
	if err := a.service.iamService.DeleteBucketPolicy(req.Bucket); err != nil {
		return nil, err
	}
	return &pb.AdminDeleteBucketPolicyResponse{}, nil
}

func (a *GRPCAdapter) AdminSimulatePolicy(ctx context.Context, req *pb.AdminSimulatePolicyRequest) (*pb.AdminSimulatePolicyResponse, error) {
	evalCtx := &iam.EvalContext{
		Principal: req.Principal,
		Action:    req.Action,
		Resource:  req.Resource,
		SourceIP:  req.SourceIp,
		Time:      time.Now(),
	}
	result := a.service.iamService.GetEvaluator().Simulate(evalCtx)
	return &pb.AdminSimulatePolicyResponse{
		Decision:   result.Decision,
		MatchedBy:  result.MatchedBy,
		PolicyType: result.PolicyType,
		Details:    result.Details,
	}, nil
}
