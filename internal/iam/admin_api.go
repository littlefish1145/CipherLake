package iam

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// AdminAPI provides HTTP endpoints for IAM administration
type AdminAPI struct {
	service *IAMService
	jwtKey  []byte
}

// NewAdminAPI creates a new admin API handler
func NewAdminAPI(service *IAMService, jwtKey []byte) *AdminAPI {
	return &AdminAPI{
		service: service,
		jwtKey:  jwtKey,
	}
}

// ServeHTTP routes admin API requests
func (a *AdminAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "groups/") && strings.HasSuffix(path, "/policies"):
		a.detachGroupPolicy(w, r)
	case r.Method == http.MethodGet && path == "roles":
		a.listRoles(w, r)
	case r.Method == http.MethodPost && path == "roles":
		a.createRole(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "roles/"):
		name := strings.TrimPrefix(path, "roles/")
		a.deleteRole(w, r, name)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "roles/") && strings.HasSuffix(path, "/policies"):
		a.attachRolePolicy(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "roles/") && strings.HasSuffix(path, "/policies"):
		a.detachRolePolicy(w, r)
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

func (a *AdminAPI) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.service.ListUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Strip secret key enc from response
	type userResponse struct {
		ID               string   `json:"id"`
		Name             string   `json:"name"`
		DisplayName      string   `json:"display_name"`
		AccessKeyCount   int      `json:"access_key_count"`
		Groups           []string `json:"groups"`
		AttachedPolicies []string `json:"attached_policies"`
		CreatedAt        string   `json:"created_at"`
	}

	var resp []userResponse
	for _, u := range users {
		resp = append(resp, userResponse{
			ID:               u.ID,
			Name:             u.Name,
			DisplayName:      u.DisplayName,
			AccessKeyCount:   len(u.AccessKeys),
			Groups:           u.Groups,
			AttachedPolicies: u.AttachedPolicies,
			CreatedAt:        u.CreatedAt.Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"users": resp})
}

func (a *AdminAPI) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	user, err := a.service.CreateUser(req.Name, req.DisplayName)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"user": map[string]interface{}{
			"id":         user.ID,
			"name":       user.Name,
			"display_name": user.DisplayName,
			"created_at": user.CreatedAt.Format(time.RFC3339),
		},
	})
}

func (a *AdminAPI) deleteUser(w http.ResponseWriter, r *http.Request, name string) {
	if err := a.service.DeleteUser(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Access Key handlers ---

func (a *AdminAPI) listAccessKeys(w http.ResponseWriter, r *http.Request, userName string) {
	keys, err := a.service.ListAccessKeys(userName)
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

	var resp []keyResponse
	for _, k := range keys {
		resp = append(resp, keyResponse{
			AccessKeyID: k.AccessKeyID,
			Status:      k.Status,
			CreatedAt:   k.CreatedAt.Format(time.RFC3339),
			Description: k.Description,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"access_keys": resp})
}

func (a *AdminAPI) createAccessKey(w http.ResponseWriter, r *http.Request, userName string) {
	var req struct {
		Description string `json:"description"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	result, err := a.service.CreateAccessKey(userName, req.Description)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	zap.L().Warn("ACCESS KEY CREATED - secret will NOT be shown again",
		zap.String("user", userName),
		zap.String("access_key_id", result.AccessKeyID))

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"access_key_id":     result.AccessKeyID,
		"secret_access_key": result.SecretAccessKey,
		"status":            result.Status,
		"created_at":        result.CreatedAt.Format(time.RFC3339),
		"warning":           "This is the only time the secret access key can be viewed or saved. Store it securely.",
	})
}

func (a *AdminAPI) deleteAccessKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserName    string `json:"user_name"`
		AccessKeyID string `json:"access_key_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.DeleteAccessKey(req.UserName, req.AccessKeyID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *AdminAPI) activateAccessKey(w http.ResponseWriter, r *http.Request) {
	accessKeyID := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/access-keys/"), "/activate"), "")
	var req struct {
		UserName string `json:"user_name"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if err := a.service.ActivateAccessKey(req.UserName, accessKeyID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

func (a *AdminAPI) deactivateAccessKey(w http.ResponseWriter, r *http.Request) {
	accessKeyID := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/access-keys/"), "/deactivate"), "")
	var req struct {
		UserName string `json:"user_name"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if err := a.service.DeactivateAccessKey(req.UserName, accessKeyID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "inactive"})
}

// --- Group handlers ---

func (a *AdminAPI) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := a.service.ListGroups()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"groups": groups})
}

func (a *AdminAPI) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	group, err := a.service.CreateGroup(req.Name, req.Description)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{"group": group})
}

func (a *AdminAPI) deleteGroup(w http.ResponseWriter, r *http.Request, name string) {
	if err := a.service.DeleteGroup(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *AdminAPI) addUserToGroup(w http.ResponseWriter, r *http.Request) {
	groupName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/users"), "")
	var req struct {
		UserName string `json:"user_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.AddUserToGroup(req.UserName, groupName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

func (a *AdminAPI) removeUserFromGroup(w http.ResponseWriter, r *http.Request) {
	groupName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/users"), "")
	var req struct {
		UserName string `json:"user_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.RemoveUserFromGroup(req.UserName, groupName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// --- Policy handlers ---

func (a *AdminAPI) listPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := a.service.ListPolicies()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"policies": policies})
}

func (a *AdminAPI) createPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Document    PolicyDocument `json:"document"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	policy, err := a.service.CreatePolicy(req.Name, req.Description, req.Document)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{"policy": policy})
}

func (a *AdminAPI) deletePolicy(w http.ResponseWriter, r *http.Request, name string) {
	if err := a.service.DeletePolicy(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *AdminAPI) attachUserPolicy(w http.ResponseWriter, r *http.Request) {
	userName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/users/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.AttachUserPolicy(userName, req.PolicyName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

func (a *AdminAPI) detachUserPolicy(w http.ResponseWriter, r *http.Request) {
	userName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/users/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.DetachUserPolicy(userName, req.PolicyName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "detached"})
}

func (a *AdminAPI) attachGroupPolicy(w http.ResponseWriter, r *http.Request) {
	groupName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.AttachGroupPolicy(groupName, req.PolicyName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

func (a *AdminAPI) detachGroupPolicy(w http.ResponseWriter, r *http.Request) {
	groupName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.DetachGroupPolicy(groupName, req.PolicyName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "detached"})
}

// --- Role handlers ---

func (a *AdminAPI) listRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := a.service.ListRoles()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"roles": roles})
}

func (a *AdminAPI) createRole(w http.ResponseWriter, r *http.Request) {
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

	role, err := a.service.CreateRole(req.Name, req.Description, req.TrustPolicy, req.MaxSessionDuration)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{"role": role})
}

func (a *AdminAPI) deleteRole(w http.ResponseWriter, r *http.Request, name string) {
	if err := a.service.DeleteRole(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *AdminAPI) attachRolePolicy(w http.ResponseWriter, r *http.Request) {
	roleName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/roles/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.AttachRolePolicy(roleName, req.PolicyName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

func (a *AdminAPI) detachRolePolicy(w http.ResponseWriter, r *http.Request) {
	roleName := strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/roles/"), "/policies"), "")
	var req struct {
		PolicyName string `json:"policy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.DetachRolePolicy(roleName, req.PolicyName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "detached"})
}

// --- Bucket Policy handlers ---

func (a *AdminAPI) getBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	bp, err := a.service.GetBucketPolicy(bucket)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"bucket_policy": bp})
}

func (a *AdminAPI) putBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Document PolicyDocument `json:"document"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := a.service.PutBucketPolicy(bucket, req.Document); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (a *AdminAPI) deleteBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := a.service.DeleteBucketPolicy(bucket); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Helpers ---

func (a *AdminAPI) simulatePolicy(w http.ResponseWriter, r *http.Request) {
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

	ctx := &EvalContext{
		Principal:  req.Principal,
		Action:     req.Action,
		Resource:   req.Resource,
		SourceIP:   req.SourceIP,
		UserAgent:  req.UserAgent,
		Time:       time.Now(),
	}

	result := a.service.GetEvaluator().Simulate(ctx)
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
