package iam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	pb "cipherlake/proto/sts"
)

// RemoteAdminAPI provides the same HTTP endpoints as AdminAPI but proxies
// all operations to sts-service via gRPC. This is used in distributed mode
// where cipherlake does not have direct access to the IAM BoltDB database.
type RemoteAdminAPI struct {
	client pb.STSServiceClient
	jwtKey []byte
}

// NewRemoteAdminAPI creates a new remote admin API handler
func NewRemoteAdminAPI(client pb.STSServiceClient, jwtKey []byte) *RemoteAdminAPI {
	return &RemoteAdminAPI{
		client: client,
		jwtKey: jwtKey,
	}
}

// ServeHTTP routes admin API requests - same routing as local AdminAPI
func (a *RemoteAdminAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// After http.StripPrefix("/iam", ...), paths come in as /users, /groups, etc.
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	// Also handle paths without /admin/ prefix (from StripPrefix("/iam"))
	path = strings.TrimPrefix(path, "/")

	switch {
	case r.Method == http.MethodGet && path == "users":
		a.listUsers(w, r)
	case r.Method == http.MethodPost && path == "users":
		a.createUser(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "users/"):
		name := strings.TrimPrefix(path, "users/")
		a.deleteUser(w, r, name)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "users/") && strings.HasSuffix(path, "/access-keys"):
		userName := strings.TrimSuffix(strings.TrimPrefix(path, "users/"), "/access-keys")
		a.listAccessKeys(w, r, userName)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "users/") && strings.HasSuffix(path, "/access-keys"):
		userName := strings.TrimSuffix(strings.TrimPrefix(path, "users/"), "/access-keys")
		a.createAccessKey(w, r, userName)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "access-keys/"):
		a.deleteAccessKey(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, "access-keys/") && strings.HasSuffix(path, "/activate"):
		a.activateAccessKey(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, "access-keys/") && strings.HasSuffix(path, "/deactivate"):
		a.deactivateAccessKey(w, r)
	case r.Method == http.MethodGet && path == "groups":
		a.listGroups(w, r)
	case r.Method == http.MethodPost && path == "groups":
		a.createGroup(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "groups/"):
		name := strings.TrimPrefix(path, "groups/")
		a.deleteGroup(w, r, name)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "groups/") && strings.HasSuffix(path, "/users"):
		a.addUserToGroup(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "groups/") && strings.HasSuffix(path, "/users"):
		a.removeUserFromGroup(w, r)
	case r.Method == http.MethodGet && path == "policies":
		a.listPolicies(w, r)
	case r.Method == http.MethodPost && path == "policies":
		a.createPolicy(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "policies/"):
		name := strings.TrimPrefix(path, "policies/")
		a.deletePolicy(w, r, name)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "users/") && strings.HasSuffix(path, "/policies"):
		a.attachUserPolicy(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "users/") && strings.HasSuffix(path, "/policies"):
		a.detachUserPolicy(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "groups/") && strings.HasSuffix(path, "/policies"):
		a.attachGroupPolicy(w, r)
	case r.Method == http.MethodGet && path == "roles":
		a.listRoles(w, r)
	case r.Method == http.MethodPost && path == "roles":
		a.createRole(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "roles/"):
		name := strings.TrimPrefix(path, "roles/")
		a.deleteRole(w, r, name)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "roles/") && strings.HasSuffix(path, "/policies"):
		a.attachRolePolicy(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "bucket-policies/"):
		bucket := strings.TrimPrefix(path, "bucket-policies/")
		a.getBucketPolicy(w, r, bucket)
	case r.Method == http.MethodPut && strings.HasPrefix(path, "bucket-policies/"):
		bucket := strings.TrimPrefix(path, "bucket-policies/")
		a.putBucketPolicy(w, r, bucket)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "bucket-policies/"):
		bucket := strings.TrimPrefix(path, "bucket-policies/")
		a.deleteBucketPolicy(w, r, bucket)
	case r.Method == http.MethodPost && path == "iam/simulate":
		a.simulatePolicy(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// --- User handlers ---

func (a *RemoteAdminAPI) listUsers(w http.ResponseWriter, r *http.Request) {
	resp, err := a.client.AdminListUsers(r.Context(), &pb.AdminListUsersRequest{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	type userResponse struct {
		ID               string   `json:"id"`
		Name             string   `json:"name"`
		DisplayName      string   `json:"display_name"`
		AccessKeyCount   int      `json:"access_key_count"`
		Groups           []string `json:"groups"`
		AttachedPolicies []string `json:"attached_policies"`
		CreatedAt        string   `json:"created_at"`
	}

	var result []userResponse
	for _, u := range resp.Users {
		result = append(result, userResponse{
			ID:               u.Id,
			Name:             u.Name,
			DisplayName:      u.DisplayName,
			AccessKeyCount:   len(u.AccessKeys),
			Groups:           u.Groups,
			AttachedPolicies: u.AttachedPolicies,
			CreatedAt:        time.Unix(u.CreatedAtUnix, 0).Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"users": result})
}

func (a *RemoteAdminAPI) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	resp, err := a.client.AdminCreateUser(r.Context(), &pb.AdminCreateUserRequest{
		Name:        req.Name,
		DisplayName: req.DisplayName,
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"user": map[string]interface{}{
			"id":           resp.User.Id,
			"name":         resp.User.Name,
			"display_name": resp.User.DisplayName,
			"created_at":   time.Unix(resp.User.CreatedAtUnix, 0).Format(time.RFC3339),
		},
	})
}

func (a *RemoteAdminAPI) deleteUser(w http.ResponseWriter, r *http.Request, name string) {
	_, err := a.client.AdminDeleteUser(r.Context(), &pb.AdminDeleteUserRequest{UserName: name})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Access Key handlers ---

func (a *RemoteAdminAPI) listAccessKeys(w http.ResponseWriter, r *http.Request, userName string) {
	resp, err := a.client.AdminListAccessKeys(r.Context(), &pb.AdminListAccessKeysRequest{UserName: userName})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	type keyResponse struct {
		AccessKeyID string `json:"access_key_id"`
		Status      string `json:"status"`
		CreatedAt   string `json:"created_at"`
		Description string `json:"description,omitempty"`
	}

	var result []keyResponse
	for _, k := range resp.AccessKeys {
		result = append(result, keyResponse{
			AccessKeyID: k.AccessKeyId,
			Status:      k.Status,
			CreatedAt:   time.Unix(k.CreatedAtUnix, 0).Format(time.RFC3339),
			Description: k.Description,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"access_keys": result})
}

func (a *RemoteAdminAPI) createAccessKey(w http.ResponseWriter, r *http.Request, userName string) {
	var req struct {
		Description string `json:"description"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	resp, err := a.client.AdminCreateAccessKey(r.Context(), &pb.AdminCreateAccessKeyRequest{
		UserName:    userName,
		Description: req.Description,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"access_key_id":     resp.AccessKeyId,
		"secret_access_key": resp.SecretAccessKey,
		"status":            resp.Status,
		"created_at":        time.Unix(resp.CreatedAtUnix, 0).Format(time.RFC3339),
		"warning":           "This is the only time the secret access key can be viewed or saved. Store it securely.",
	})
}

func (a *RemoteAdminAPI) deleteAccessKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserName    string `json:"user_name"`
		AccessKeyID string `json:"access_key_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	_, err := a.client.AdminDeleteAccessKey(r.Context(), &pb.AdminDeleteAccessKeyRequest{
		UserName:    req.UserName,
		AccessKeyId: req.AccessKeyID,
	})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *RemoteAdminAPI) activateAccessKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented in remote mode"})
}

func (a *RemoteAdminAPI) deactivateAccessKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented in remote mode"})
}

// --- Group handlers ---

func (a *RemoteAdminAPI) listGroups(w http.ResponseWriter, r *http.Request) {
	resp, err := a.client.AdminListGroups(r.Context(), &pb.AdminListGroupsRequest{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var groups []interface{}
	for _, g := range resp.Groups {
		groups = append(groups, map[string]interface{}{
			"name":              g.Name,
			"description":       g.Description,
			"users":             g.Users,
			"attached_policies": g.AttachedPolicies,
			"created_at":        time.Unix(g.CreatedAtUnix, 0).Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"groups": groups})
}

func (a *RemoteAdminAPI) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	resp, err := a.client.AdminCreateGroup(r.Context(), &pb.AdminCreateGroupRequest{
		Name:        req.Name,
		Description: req.Description,
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"group": map[string]interface{}{
			"name":        resp.Name,
			"description": resp.Description,
			"created_at":  time.Unix(resp.CreatedAtUnix, 0).Format(time.RFC3339),
		},
	})
}

func (a *RemoteAdminAPI) deleteGroup(w http.ResponseWriter, r *http.Request, name string) {
	_, err := a.client.AdminDeleteGroup(r.Context(), &pb.AdminDeleteGroupRequest{GroupName: name})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *RemoteAdminAPI) addUserToGroup(w http.ResponseWriter, r *http.Request) {
	groupName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/users"), "")
	var req struct {
		UserName string `json:"user_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	_, err := a.client.AdminAddUserToGroup(r.Context(), &pb.AdminAddUserToGroupRequest{
		UserName:  req.UserName,
		GroupName: groupName,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

func (a *RemoteAdminAPI) removeUserFromGroup(w http.ResponseWriter, r *http.Request) {
	groupName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/users"), "")
	var req struct {
		UserName string `json:"user_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	_, err := a.client.AdminRemoveUserFromGroup(r.Context(), &pb.AdminRemoveUserFromGroupRequest{
		UserName:  req.UserName,
		GroupName: groupName,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// --- Policy handlers ---

func (a *RemoteAdminAPI) listPolicies(w http.ResponseWriter, r *http.Request) {
	resp, err := a.client.AdminListPolicies(r.Context(), &pb.AdminListPoliciesRequest{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var policies []interface{}
	for _, p := range resp.Policies {
		policies = append(policies, map[string]interface{}{
			"arn":         p.Arn,
			"name":        p.Name,
			"description": p.Description,
			"type":        p.Type,
			"created_at":  time.Unix(p.CreatedAtUnix, 0).Format(time.RFC3339),
			"updated_at":  time.Unix(p.UpdatedAtUnix, 0).Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"policies": policies})
}

func (a *RemoteAdminAPI) createPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Document    PolicyDocument `json:"document"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	docJSON, err := json.Marshal(req.Document)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid policy document"})
		return
	}

	resp, err := a.client.AdminCreatePolicy(r.Context(), &pb.AdminCreatePolicyRequest{
		Name:         req.Name,
		Description:  req.Description,
		DocumentJson: string(docJSON),
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"policy": map[string]interface{}{
			"arn":  resp.Arn,
			"name": resp.Name,
		},
	})
}

func (a *RemoteAdminAPI) deletePolicy(w http.ResponseWriter, r *http.Request, name string) {
	_, err := a.client.AdminDeletePolicy(r.Context(), &pb.AdminDeletePolicyRequest{PolicyName: name})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *RemoteAdminAPI) attachUserPolicy(w http.ResponseWriter, r *http.Request) {
	userName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/users/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	_, err := a.client.AdminAttachUserPolicy(r.Context(), &pb.AdminAttachUserPolicyRequest{
		UserName:   userName,
		PolicyName: req.PolicyName,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

func (a *RemoteAdminAPI) detachUserPolicy(w http.ResponseWriter, r *http.Request) {
	userName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/users/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	_, err := a.client.AdminDetachUserPolicy(r.Context(), &pb.AdminDetachUserPolicyRequest{
		UserName:   userName,
		PolicyName: req.PolicyName,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "detached"})
}

func (a *RemoteAdminAPI) attachGroupPolicy(w http.ResponseWriter, r *http.Request) {
	groupName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	_, err := a.client.AdminAttachGroupPolicy(r.Context(), &pb.AdminAttachGroupPolicyRequest{
		GroupName:  groupName,
		PolicyName: req.PolicyName,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

// --- Role handlers ---

func (a *RemoteAdminAPI) listRoles(w http.ResponseWriter, r *http.Request) {
	resp, err := a.client.AdminListRoles(r.Context(), &pb.AdminListRolesRequest{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var roles []interface{}
	for _, r := range resp.Roles {
		roles = append(roles, map[string]interface{}{
			"name":                r.Name,
			"description":         r.Description,
			"permission_policies": r.PermissionPolicies,
			"created_at":          time.Unix(r.CreatedAtUnix, 0).Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"roles": roles})
}

func (a *RemoteAdminAPI) createRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name               string         `json:"name"`
		Description        string         `json:"description"`
		TrustPolicy        PolicyDocument `json:"trust_policy"`
		MaxSessionDuration int            `json:"max_session_duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	trustJSON, err := json.Marshal(req.TrustPolicy)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid trust policy"})
		return
	}

	resp, err := a.client.AdminCreateRole(r.Context(), &pb.AdminCreateRoleRequest{
		Name:               req.Name,
		Description:        req.Description,
		TrustPolicyJson:    string(trustJSON),
		MaxSessionDuration: int32(req.MaxSessionDuration),
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"role": map[string]interface{}{"name": resp.Name},
	})
}

func (a *RemoteAdminAPI) deleteRole(w http.ResponseWriter, r *http.Request, name string) {
	_, err := a.client.AdminDeleteRole(r.Context(), &pb.AdminDeleteRoleRequest{RoleName: name})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *RemoteAdminAPI) attachRolePolicy(w http.ResponseWriter, r *http.Request) {
	roleName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/roles/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	_, err := a.client.AdminAttachRolePolicy(r.Context(), &pb.AdminAttachRolePolicyRequest{
		RoleName:   roleName,
		PolicyName: req.PolicyName,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

// --- Bucket Policy handlers ---

func (a *RemoteAdminAPI) getBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	resp, err := a.client.AdminGetBucketPolicy(r.Context(), &pb.AdminGetBucketPolicyRequest{Bucket: bucket})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	var doc interface{}
	json.Unmarshal([]byte(resp.DocumentJson), &doc)
	writeJSON(w, http.StatusOK, map[string]interface{}{"bucket_policy": map[string]interface{}{"document": doc}})
}

func (a *RemoteAdminAPI) putBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Document PolicyDocument `json:"document"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	docJSON, _ := json.Marshal(req.Document)
	_, err := a.client.AdminPutBucketPolicy(r.Context(), &pb.AdminPutBucketPolicyRequest{
		Bucket:       bucket,
		DocumentJson: string(docJSON),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (a *RemoteAdminAPI) deleteBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	_, err := a.client.AdminDeleteBucketPolicy(r.Context(), &pb.AdminDeleteBucketPolicyRequest{Bucket: bucket})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Helpers ---

func (a *RemoteAdminAPI) simulatePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Principal string `json:"principal"`
		Action    string `json:"action"`
		Resource  string `json:"resource"`
		SourceIP  string `json:"source_ip"`
		UserAgent string `json:"user_agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Principal == "" || req.Action == "" || req.Resource == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "principal, action, and resource are required"})
		return
	}

	resp, err := a.client.AdminSimulatePolicy(r.Context(), &pb.AdminSimulatePolicyRequest{
		Principal: req.Principal,
		Action:    req.Action,
		Resource:  req.Resource,
		SourceIp:  req.SourceIP,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"decision":    resp.Decision,
		"matched_by":  resp.MatchedBy,
		"policy_type": resp.PolicyType,
		"details":     resp.Details,
	})
}

// compile-time check
var _ http.Handler = (*RemoteAdminAPI)(nil)

// remoteAdminAPIError is a helper to format gRPC errors
func remoteAdminAPIError(err error) map[string]string {
	msg := err.Error()
	// Strip gRPC prefix if present
	if idx := strings.Index(msg, "desc ="); idx != -1 {
		msg = strings.TrimSpace(msg[idx+7:])
	}
	return map[string]string{"error": msg}
}

// unused but kept for interface compatibility
var _ = fmt.Sprintf
var _ context.Context
