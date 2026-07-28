package iam

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// IAMService provides the core IAM operations
type IAMService struct {
	store     *IAMStore
	masterKey *MasterKey
	evaluator *PolicyEvaluator
}

// NewIAMService creates a new IAM service
func NewIAMService(store *IAMStore, masterKey *MasterKey) *IAMService {
	return &IAMService{
		store:     store,
		masterKey: masterKey,
		evaluator: NewPolicyEvaluator(store),
	}
}

// GetEvaluator returns the policy evaluator
func (s *IAMService) GetEvaluator() *PolicyEvaluator {
	return s.evaluator
}

// GetStore returns the IAM store
func (s *IAMService) GetStore() *IAMStore {
	return s.store
}

// --- User operations ---

// CreateUser creates a new IAM user
func (s *IAMService) CreateUser(name, displayName string) (*IAMUser, error) {
	// Check if user already exists
	if _, err := s.store.GetUser(name); err == nil {
		return nil, fmt.Errorf("user %s already exists", name)
	}

	user := &IAMUser{
		ID:               generateID("u"),
		Name:             name,
		DisplayName:      displayName,
		AccessKeys:       []AccessKey{},
		Groups:           []string{},
		AttachedPolicies: []string{},
		InlinePolicies:   []PolicyDocument{},
		CreatedAt:        time.Now(),
	}

	if err := s.store.PutUser(user); err != nil {
		return nil, fmt.Errorf("failed to save user: %w", err)
	}

	zap.L().Info("created IAM user", zap.String("name", name))
	return user, nil
}

// DeleteUser deletes an IAM user
func (s *IAMService) DeleteUser(name string) error {
	// Remove user from all groups
	user, err := s.store.GetUser(name)
	if err != nil {
		return fmt.Errorf("user %s not found", name)
	}

	for _, groupName := range user.Groups {
		_ = s.RemoveUserFromGroup(name, groupName)
	}

	return s.store.DeleteUser(name)
}

// GetUser gets a user by name
func (s *IAMService) GetUser(name string) (*IAMUser, error) {
	return s.store.GetUser(name)
}

// ListUsers lists all users
func (s *IAMService) ListUsers() ([]*IAMUser, error) {
	return s.store.ListUsers()
}

// --- Access Key operations ---

// CreateAccessKey creates a new access key for a user
// The secret key is returned in plaintext ONLY this one time
func (s *IAMService) CreateAccessKey(userName, description string) (*CreateAccessKeyResult, error) {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return nil, fmt.Errorf("user %s not found", userName)
	}

	// Generate Access Key ID (AKIA + 16 random hex chars = 20 chars total)
	accessKeyID := AccessKeyIDPrefix + generateRandomString(16)

	// Generate Secret Access Key (40 random chars)
	secretKey := generateRandomString(40)

	// Encrypt secret key for storage
	secretKeyEnc, err := s.masterKey.Encrypt([]byte(secretKey))
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt secret key: %w", err)
	}

	ak := AccessKey{
		AccessKeyID:  accessKeyID,
		SecretKeyEnc: secretKeyEnc,
		Status:       AccessKeyActive,
		CreatedAt:    time.Now(),
		Description:  description,
	}

	user.AccessKeys = append(user.AccessKeys, ak)
	if err := s.store.PutUser(user); err != nil {
		return nil, fmt.Errorf("failed to save user: %w", err)
	}

	zap.L().Info("created access key for user",
		zap.String("user", userName),
		zap.String("access_key_id", accessKeyID))

	return &CreateAccessKeyResult{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretKey, // Only returned once
		Status:          AccessKeyActive,
		CreatedAt:       ak.CreatedAt,
	}, nil
}

// DeleteAccessKey deletes an access key
func (s *IAMService) DeleteAccessKey(userName, accessKeyID string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return fmt.Errorf("user %s not found", userName)
	}

	for i, ak := range user.AccessKeys {
		if ak.AccessKeyID == accessKeyID {
			user.AccessKeys = append(user.AccessKeys[:i], user.AccessKeys[i+1:]...)
			return s.store.PutUser(user)
		}
	}

	return fmt.Errorf("access key %s not found for user %s", accessKeyID, userName)
}

// ListAccessKeys lists access keys for a user
func (s *IAMService) ListAccessKeys(userName string) ([]AccessKey, error) {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return nil, err
	}
	return user.AccessKeys, nil
}

// ActivateAccessKey activates an access key
func (s *IAMService) ActivateAccessKey(userName, accessKeyID string) error {
	return s.setAccessKeyStatus(userName, accessKeyID, AccessKeyActive)
}

// DeactivateAccessKey deactivates an access key
func (s *IAMService) DeactivateAccessKey(userName, accessKeyID string) error {
	return s.setAccessKeyStatus(userName, accessKeyID, AccessKeyInactive)
}

func (s *IAMService) setAccessKeyStatus(userName, accessKeyID, status string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return err
	}

	for i := range user.AccessKeys {
		if user.AccessKeys[i].AccessKeyID == accessKeyID {
			user.AccessKeys[i].Status = status
			return s.store.PutUser(user)
		}
	}

	return fmt.Errorf("access key %s not found", accessKeyID)
}

// DecryptSecretKey decrypts a secret key (used internally for SigV4 verification)
func (s *IAMService) DecryptSecretKey(encryptedSecret []byte) (string, error) {
	plaintext, err := s.masterKey.Decrypt(encryptedSecret)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt secret key: %w", err)
	}
	return string(plaintext), nil
}

// GetUserByAccessKeyID looks up a user by their access key ID
func (s *IAMService) GetUserByAccessKeyID(accessKeyID string) (*IAMUser, *AccessKey, error) {
	user, err := s.store.GetUserByAccessKey(accessKeyID)
	if err != nil {
		return nil, nil, fmt.Errorf("no user found for access key %s", accessKeyID)
	}

	for i := range user.AccessKeys {
		if user.AccessKeys[i].AccessKeyID == accessKeyID {
			return user, &user.AccessKeys[i], nil
		}
	}

	return nil, nil, fmt.Errorf("access key %s not found", accessKeyID)
}

// --- Group operations ---

// CreateGroup creates a new IAM group
func (s *IAMService) CreateGroup(name, description string) (*IAMGroup, error) {
	if _, err := s.store.GetGroup(name); err == nil {
		return nil, fmt.Errorf("group %s already exists", name)
	}

	group := &IAMGroup{
		Name:             name,
		Description:      description,
		Users:            []string{},
		AttachedPolicies: []string{},
		CreatedAt:        time.Now(),
	}

	if err := s.store.PutGroup(group); err != nil {
		return nil, err
	}

	zap.L().Info("created IAM group", zap.String("name", name))
	return group, nil
}

// DeleteGroup deletes an IAM group
func (s *IAMService) DeleteGroup(name string) error {
	group, err := s.store.GetGroup(name)
	if err != nil {
		return err
	}

	// Remove group from all member users
	for _, userName := range group.Users {
		user, err := s.store.GetUser(userName)
		if err != nil {
			continue
		}
		user.Groups = removeString(user.Groups, name)
		_ = s.store.PutUser(user)
	}

	return s.store.DeleteGroup(name)
}

// AddUserToGroup adds a user to a group
func (s *IAMService) AddUserToGroup(userName, groupName string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return fmt.Errorf("user %s not found", userName)
	}

	group, err := s.store.GetGroup(groupName)
	if err != nil {
		return fmt.Errorf("group %s not found", groupName)
	}

	// Add to user's groups
	if !containsString(user.Groups, groupName) {
		user.Groups = append(user.Groups, groupName)
		if err := s.store.PutUser(user); err != nil {
			return err
		}
	}

	// Add to group's users
	if !containsString(group.Users, userName) {
		group.Users = append(group.Users, userName)
		if err := s.store.PutGroup(group); err != nil {
			return err
		}
	}

	return nil
}

// RemoveUserFromGroup removes a user from a group
func (s *IAMService) RemoveUserFromGroup(userName, groupName string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return err
	}

	group, err := s.store.GetGroup(groupName)
	if err != nil {
		return err
	}

	user.Groups = removeString(user.Groups, groupName)
	group.Users = removeString(group.Users, userName)

	if err := s.store.PutUser(user); err != nil {
		return err
	}
	return s.store.PutGroup(group)
}

// ListGroups lists all groups
func (s *IAMService) ListGroups() ([]*IAMGroup, error) {
	return s.store.ListGroups()
}

// --- Policy operations ---

// CreatePolicy creates a new managed IAM policy
func (s *IAMService) CreatePolicy(name, description string, document PolicyDocument) (*IAMPolicy, error) {
	if _, err := s.store.GetPolicy(name); err == nil {
		return nil, fmt.Errorf("policy %s already exists", name)
	}

	policy := &IAMPolicy{
		ARN:         MakePolicyARN(name),
		Name:        name,
		Description: description,
		Type:        "Custom",
		Document:    document,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	if err := s.store.PutPolicy(policy); err != nil {
		return nil, err
	}

	zap.L().Info("created IAM policy", zap.String("name", name))
	return policy, nil
}

// DeletePolicy deletes a managed IAM policy
func (s *IAMService) DeletePolicy(name string) error {
	if err := s.ensurePolicyNotAttached(name); err != nil {
		return err
	}
	return s.store.DeletePolicy(name)
}

func (s *IAMService) ensurePolicyNotAttached(name string) error {
	policyRefs := map[string]struct{}{
		name:                {},
		MakePolicyARN(name): {},
	}

	users, err := s.store.ListUsers()
	if err != nil {
		return fmt.Errorf("failed to list users for policy attachment check: %w", err)
	}
	for _, user := range users {
		if hasPolicyRef(policyRefs, user.AttachedPolicies) {
			return fmt.Errorf("policy %s is attached to user %s", name, user.Name)
		}
		if _, ok := policyRefs[user.PermissionBoundary]; ok {
			return fmt.Errorf("policy %s is used as permission boundary by user %s", name, user.Name)
		}
	}

	groups, err := s.store.ListGroups()
	if err != nil {
		return fmt.Errorf("failed to list groups for policy attachment check: %w", err)
	}
	for _, group := range groups {
		if hasPolicyRef(policyRefs, group.AttachedPolicies) {
			return fmt.Errorf("policy %s is attached to group %s", name, group.Name)
		}
	}

	roles, err := s.store.ListRoles()
	if err != nil {
		return fmt.Errorf("failed to list roles for policy attachment check: %w", err)
	}
	for _, role := range roles {
		if hasPolicyRef(policyRefs, role.PermissionPolicies) {
			return fmt.Errorf("policy %s is attached to role %s", name, role.Name)
		}
	}

	return nil
}

func hasPolicyRef(policyRefs map[string]struct{}, refs []string) bool {
	for _, ref := range refs {
		if _, ok := policyRefs[ref]; ok {
			return true
		}
	}
	return false
}

// ListPolicies lists all managed policies
func (s *IAMService) ListPolicies() ([]*IAMPolicy, error) {
	return s.store.ListPolicies()
}

// AttachUserPolicy attaches a managed policy to a user
func (s *IAMService) AttachUserPolicy(userName, policyName string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return err
	}

	// Verify policy exists
	if _, err := s.store.GetPolicy(policyName); err != nil {
		return fmt.Errorf("policy %s not found", policyName)
	}

	if !containsString(user.AttachedPolicies, policyName) {
		user.AttachedPolicies = append(user.AttachedPolicies, policyName)
		return s.store.PutUser(user)
	}

	return nil
}

// DetachUserPolicy detaches a managed policy from a user
func (s *IAMService) DetachUserPolicy(userName, policyName string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return err
	}

	user.AttachedPolicies = removeString(user.AttachedPolicies, policyName)
	return s.store.PutUser(user)
}

// AttachGroupPolicy attaches a managed policy to a group
func (s *IAMService) AttachGroupPolicy(groupName, policyName string) error {
	group, err := s.store.GetGroup(groupName)
	if err != nil {
		return err
	}

	if _, err := s.store.GetPolicy(policyName); err != nil {
		return fmt.Errorf("policy %s not found", policyName)
	}

	if !containsString(group.AttachedPolicies, policyName) {
		group.AttachedPolicies = append(group.AttachedPolicies, policyName)
		return s.store.PutGroup(group)
	}

	return nil
}

// DetachGroupPolicy detaches a managed policy from a group
func (s *IAMService) DetachGroupPolicy(groupName, policyName string) error {
	group, err := s.store.GetGroup(groupName)
	if err != nil {
		return err
	}

	group.AttachedPolicies = removeString(group.AttachedPolicies, policyName)
	return s.store.PutGroup(group)
}

// PutUserInlinePolicy adds or replaces an inline policy for a user
func (s *IAMService) PutUserInlinePolicy(userName, policyName string, document PolicyDocument) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return err
	}

	document.Id = policyName
	// Replace if exists, otherwise append
	for i, p := range user.InlinePolicies {
		if p.Id == policyName {
			user.InlinePolicies[i] = document
			return s.store.PutUser(user)
		}
	}

	user.InlinePolicies = append(user.InlinePolicies, document)
	return s.store.PutUser(user)
}

// DeleteUserInlinePolicy removes an inline policy from a user
func (s *IAMService) DeleteUserInlinePolicy(userName, policyName string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return err
	}

	for i, p := range user.InlinePolicies {
		if p.Id == policyName {
			user.InlinePolicies = append(user.InlinePolicies[:i], user.InlinePolicies[i+1:]...)
			return s.store.PutUser(user)
		}
	}

	return fmt.Errorf("inline policy %s not found", policyName)
}

// --- Role operations ---

// CreateRole creates a new IAM role
func (s *IAMService) CreateRole(name, description string, trustPolicy PolicyDocument, maxSessionDuration int) (*IAMRole, error) {
	if _, err := s.store.GetRole(name); err == nil {
		return nil, fmt.Errorf("role %s already exists", name)
	}

	if maxSessionDuration <= 0 {
		maxSessionDuration = DefaultMaxSessionDuration
	}

	role := &IAMRole{
		Name:               name,
		Description:        description,
		TrustPolicy:        trustPolicy,
		PermissionPolicies: []string{},
		MaxSessionDuration: maxSessionDuration,
		CreatedAt:          time.Now(),
	}

	if err := s.store.PutRole(role); err != nil {
		return nil, err
	}

	zap.L().Info("created IAM role", zap.String("name", name))
	return role, nil
}

// DeleteRole deletes an IAM role
func (s *IAMService) DeleteRole(name string) error {
	return s.store.DeleteRole(name)
}

// AttachRolePolicy attaches a managed policy to a role
func (s *IAMService) AttachRolePolicy(roleName, policyName string) error {
	role, err := s.store.GetRole(roleName)
	if err != nil {
		return err
	}

	if _, err := s.store.GetPolicy(policyName); err != nil {
		return fmt.Errorf("policy %s not found", policyName)
	}

	if !containsString(role.PermissionPolicies, policyName) {
		role.PermissionPolicies = append(role.PermissionPolicies, policyName)
		return s.store.PutRole(role)
	}

	return nil
}

// DetachRolePolicy detaches a managed policy from a role
func (s *IAMService) DetachRolePolicy(roleName, policyName string) error {
	role, err := s.store.GetRole(roleName)
	if err != nil {
		return err
	}

	role.PermissionPolicies = removeString(role.PermissionPolicies, policyName)
	return s.store.PutRole(role)
}

// GetRole gets a role by name
func (s *IAMService) GetRole(name string) (*IAMRole, error) {
	return s.store.GetRole(name)
}

// ListRoles lists all roles
func (s *IAMService) ListRoles() ([]*IAMRole, error) {
	return s.store.ListRoles()
}

// --- Bucket Policy operations ---

// PutBucketPolicy sets a bucket policy
func (s *IAMService) PutBucketPolicy(bucket string, document PolicyDocument) error {
	bp := &BucketPolicy{
		Bucket:    bucket,
		Document:  document,
		UpdatedAt: time.Now(),
	}
	return s.store.PutBucketPolicy(bp)
}

// DeleteBucketPolicy removes a bucket policy
func (s *IAMService) DeleteBucketPolicy(bucket string) error {
	return s.store.DeleteBucketPolicy(bucket)
}

// GetBucketPolicy gets a bucket policy
func (s *IAMService) GetBucketPolicy(bucket string) (*BucketPolicy, error) {
	return s.store.GetBucketPolicy(bucket)
}

// --- STS operations ---

// AssumeRole creates temporary credentials for a role assumption
func (s *IAMService) AssumeRole(req *AssumeRoleRequest, callerARN string) (*TemporaryCredential, error) {
	// Extract role name from ARN
	roleName := extractRoleNameFromARN(req.RoleARN)
	if roleName == "" {
		return nil, fmt.Errorf("invalid role ARN: %s", req.RoleARN)
	}

	role, err := s.store.GetRole(roleName)
	if err != nil {
		return nil, fmt.Errorf("role %s not found", roleName)
	}

	// Evaluate trust policy - check if caller is allowed to assume this role
	trustCtx := &EvalContext{
		Principal: callerARN,
		Action:    "sts:AssumeRole",
		Resource:  req.RoleARN,
		Time:      time.Now(),
	}

	trustResult := s.evaluator.Evaluate(trustCtx)
	// Also check the role's trust policy directly
	if len(role.TrustPolicy.Statement) > 0 {
		for _, stmt := range role.TrustPolicy.Statement {
			if stmt.Effect == EffectAllow && s.evaluator.statementMatches(&stmt, trustCtx) {
				trustResult = &EvalResult{Decision: DecisionAllow}
				break
			}
		}
	}

	if trustResult.Decision != DecisionAllow {
		return nil, fmt.Errorf("access denied: caller %s is not authorized to assume role %s", callerARN, roleName)
	}

	// Determine session duration
	duration := role.MaxSessionDuration
	if req.DurationSeconds > 0 && req.DurationSeconds <= role.MaxSessionDuration {
		duration = req.DurationSeconds
	}

	// Generate temporary credentials
	tempAccessKeyID := "ASIA" + generateRandomString(16) // ASIA prefix for temporary credentials
	tempSecretKey := generateRandomString(40)
	sessionToken := generateRandomString(64)

	// Store temp credential
	cred := &TemporaryCredential{
		AccessKeyID:     tempAccessKeyID,
		SecretAccessKey: tempSecretKey,
		SessionToken:    sessionToken,
		Expiration:      time.Now().Add(time.Duration(duration) * time.Second),
	}

	if err := s.store.PutTempCredential(cred); err != nil {
		return nil, fmt.Errorf("failed to store temp credential: %w", err)
	}

	zap.L().Info("assumed role",
		zap.String("role", roleName),
		zap.String("caller", callerARN),
		zap.String("session", req.RoleSessionName),
		zap.Int("duration", duration))

	return cred, nil
}

// GetSessionToken creates temporary credentials for the current user
func (s *IAMService) GetSessionToken(callerARN string, durationSeconds int) (*TemporaryCredential, error) {
	if durationSeconds <= 0 {
		durationSeconds = 3600 // 1 hour default
	}
	if durationSeconds > 129600 { // 36 hours max
		durationSeconds = 129600
	}

	tempAccessKeyID := "ASIA" + generateRandomString(16)
	tempSecretKey := generateRandomString(40)
	sessionToken := generateRandomString(64)

	cred := &TemporaryCredential{
		AccessKeyID:     tempAccessKeyID,
		SecretAccessKey: tempSecretKey,
		SessionToken:    sessionToken,
		Expiration:      time.Now().Add(time.Duration(durationSeconds) * time.Second),
	}

	if err := s.store.PutTempCredential(cred); err != nil {
		return nil, fmt.Errorf("failed to store temp credential: %w", err)
	}

	return cred, nil
}

// GetFederationToken creates temporary credentials for a federated user
func (s *IAMService) GetFederationToken(name, callerARN string, durationSeconds int, policy *PolicyDocument) (*TemporaryCredential, error) {
	if durationSeconds <= 0 {
		durationSeconds = 3600
	}
	if durationSeconds > 43200 { // 12 hours max
		durationSeconds = 43200
	}

	tempAccessKeyID := "ASIA" + generateRandomString(16)
	tempSecretKey := generateRandomString(40)
	sessionToken := generateRandomString(64)

	cred := &TemporaryCredential{
		AccessKeyID:     tempAccessKeyID,
		SecretAccessKey: tempSecretKey,
		SessionToken:    sessionToken,
		Expiration:      time.Now().Add(time.Duration(durationSeconds) * time.Second),
	}

	if err := s.store.PutTempCredential(cred); err != nil {
		return nil, fmt.Errorf("failed to store temp credential: %w", err)
	}

	return cred, nil
}

// GetTempCredentialByAccessKeyID retrieves a temp credential by access key ID
func (s *IAMService) GetTempCredentialByAccessKeyID(accessKeyID string) (*TemporaryCredential, error) {
	return s.store.GetTempCredential(accessKeyID)
}

// --- Policy evaluation ---

// EvaluateAccess evaluates whether a request is allowed
func (s *IAMService) EvaluateAccess(ctx *EvalContext) *EvalResult {
	return s.evaluator.Evaluate(ctx)
}

// --- Admin initialization ---

// InitializeAdmin creates the admin user if it doesn't exist
// Returns the initial access key (shown only once)
func (s *IAMService) InitializeAdmin() (*CreateAccessKeyResult, error) {
	adminUser, err := s.store.GetUser("admin")
	if err == nil && adminUser != nil && len(adminUser.AccessKeys) > 0 {
		return nil, nil // Admin already exists
	}

	// Create admin user
	if adminUser == nil {
		adminUser, err = s.CreateUser("admin", "Administrator")
		if err != nil {
			return nil, fmt.Errorf("failed to create admin user: %w", err)
		}
	}

	// Create admin access key
	result, err := s.CreateAccessKey("admin", "Initial admin key")
	if err != nil {
		return nil, fmt.Errorf("failed to create admin access key: %w", err)
	}

	// Create and attach admin policy
	adminPolicy := PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	if _, err := s.CreatePolicy("AdministratorAccess", "Full access to all resources", adminPolicy); err != nil {
		// Policy might already exist
		zap.L().Warn("admin policy creation skipped", zap.Error(err))
	}

	if err := s.AttachUserPolicy("admin", "AdministratorAccess"); err != nil {
		zap.L().Warn("failed to attach admin policy", zap.Error(err))
	}

	return result, nil
}

// --- Helper functions ---

func generateRandomString(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:length]
}

func generateID(prefix string) string {
	return prefix + "_" + generateRandomString(12)
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	for i, item := range slice {
		if item == s {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func extractRoleNameFromARN(arn string) string {
	// arn:cipherlake:iam:::role/RoleName -> RoleName
	if !strings.HasPrefix(arn, ARNPrefix) {
		return ""
	}
	parts := strings.Split(arn, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}
