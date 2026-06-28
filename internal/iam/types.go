package iam

import (
	"strings"
	"time"
)

// ARN prefix for Nexus resources
const (
	ARNPrefix    = "arn:nexus:"
	ServiceS3    = "s3"
	ServiceIAM   = "iam"
	ServiceSTS   = "sts"
	ServiceVector = "vector"

	// Access key ID prefix (AWS compatible)
	AccessKeyIDPrefix = "AKIA"

	// Access key status
	AccessKeyActive   = "Active"
	AccessKeyInactive = "Inactive"

	// Policy effect
	EffectAllow = "Allow"
	EffectDeny  = "Deny"

	// Policy version
	PolicyVersion = "2012-10-17"

	// Default max session duration for roles (1 hour)
	DefaultMaxSessionDuration = 3600
)

// AccessKey represents an AWS-compatible access key pair
type AccessKey struct {
	AccessKeyID     string     `json:"access_key_id"`
	SecretKeyEnc    []byte     `json:"secret_key_enc"`    // AES-256-GCM encrypted
	Status          string     `json:"status"`             // Active, Inactive
	CreatedAt       time.Time  `json:"created_at"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	Description     string     `json:"description,omitempty"`
}

// IAMUser represents an IAM user
type IAMUser struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	DisplayName       string            `json:"display_name,omitempty"`
	AccessKeys        []AccessKey       `json:"access_keys"`
	Groups            []string          `json:"groups"`
	AttachedPolicies  []string          `json:"attached_policies"` // Policy ARNs or names
	InlinePolicies    []PolicyDocument  `json:"inline_policies"`
	PermissionBoundary string           `json:"permission_boundary,omitempty"` // Policy name reference
	PasswordHash      string            `json:"password_hash,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	LastActivityAt    *time.Time        `json:"last_activity_at,omitempty"`
}

// IAMGroup represents an IAM group
type IAMGroup struct {
	Name             string   `json:"name"`
	Description      string   `json:"description,omitempty"`
	Users            []string `json:"users"`
	AttachedPolicies []string `json:"attached_policies"`
	CreatedAt        time.Time `json:"created_at"`
}

// IAMRole represents an IAM role for STS AssumeRole
type IAMRole struct {
	Name               string         `json:"name"`
	Description        string         `json:"description,omitempty"`
	TrustPolicy        PolicyDocument `json:"trust_policy"`
	PermissionPolicies []string       `json:"permission_policies"`
	MaxSessionDuration int            `json:"max_session_duration"` // seconds
	Path               string         `json:"path,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
}

// IAMPolicy represents a managed IAM policy
type IAMPolicy struct {
	ARN         string         `json:"arn"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Type        string         `json:"type"` // AWS (built-in) or Custom
	Document    PolicyDocument `json:"document"`
	Path        string         `json:"path,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// BucketPolicy represents an S3 bucket resource policy
type BucketPolicy struct {
	Bucket    string         `json:"bucket"`
	Document  PolicyDocument `json:"document"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// PolicyDocument represents an AWS IAM Policy Document
type PolicyDocument struct {
	Version   string      `json:"version"`
	Id        string      `json:"id,omitempty"`
	Statement []Statement `json:"statement"`
}

// Statement represents a single policy statement
type Statement struct {
	Sid       string                 `json:"sid,omitempty"`
	Effect    string                 `json:"effect"`
	Action    StringOrSlice          `json:"action"`
	NotAction StringOrSlice          `json:"not_action,omitempty"`
	Resource  StringOrSlice          `json:"resource"`
	NotResource StringOrSlice        `json:"not_resource,omitempty"`
	Principal *Principal             `json:"principal,omitempty"`
	NotPrincipal *Principal          `json:"not_principal,omitempty"`
	Condition  map[string]map[string]interface{} `json:"condition,omitempty"`
}

// Principal represents a policy principal (for bucket policies)
type Principal struct {
	AWS       StringOrSlice `json:"aws,omitempty"`
	Service   StringOrSlice `json:"service,omitempty"`
	Federated StringOrSlice `json:"federated,omitempty"`
}

// StringOrSlice handles JSON fields that can be either a string or array of strings
type StringOrSlice []string

// TemporaryCredential represents STS temporary credentials
type TemporaryCredential struct {
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	SessionToken    string    `json:"session_token"`
	Expiration      time.Time `json:"expiration"`
}

// AssumeRoleRequest represents an STS AssumeRole request
type AssumeRoleRequest struct {
	RoleARN          string `json:"role_arn"`
	RoleSessionName  string `json:"role_session_name"`
	DurationSeconds  int    `json:"duration_seconds,omitempty"`
	ExternalID       string `json:"external_id,omitempty"`
	Policy           string `json:"policy,omitempty"` // inline policy to scope down
}

// GetSessionTokenRequest represents an STS GetSessionToken request
type GetSessionTokenRequest struct {
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	SerialNumber    string `json:"serial_number,omitempty"`
	TokenCode       string `json:"token_code,omitempty"`
}

// GetFederationTokenRequest represents an STS GetFederationToken request
type GetFederationTokenRequest struct {
	Name            string `json:"name"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	Policy          string `json:"policy,omitempty"`
}

// EvalContext is the context for policy evaluation
type EvalContext struct {
	Principal     string            // User ARN or anonymous
	Action        string            // e.g., "s3:GetObject"
	Resource      string            // e.g., "arn:nexus:s3:::images/photo.png"
	Conditions    map[string]string // key-value conditions for evaluation
	SourceIP      string            // client IP
	Time          time.Time         // request time
	UserAgent     string            // client User-Agent
	PrincipalType string            // e.g., "User", "Role", "Federated"
	ResourceTags  map[string]string // tags on the resource
	PrincipalTags map[string]string // tags on the principal
	RequestTags   map[string]string // tags in the request
	S3Prefix      string            // S3 prefix for list operations
	S3ACL         string            // S3 ACL header value
}

// EvalResult is the result of policy evaluation
type EvalResult struct {
	Decision    Decision // Allow, Deny, ImplicitDeny
	MatchedBy   string   // Which policy/statement matched
	PolicyType  string   // "identity", "boundary", "scp", "resource"
	PolicyName  string   // Name of the policy that matched
	Details     string   // Human-readable explanation
}

// Decision represents an authorization decision
type Decision int

const (
	DecisionDeny Decision = iota
	DecisionAllow
	DecisionImplicitDeny
)

// String returns the string representation of a Decision
func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "Allow"
	case DecisionDeny:
		return "Deny"
	case DecisionImplicitDeny:
		return "ImplicitDeny"
	default:
		return "Unknown"
	}
}

// SimulateResponse is the response for policy simulation
type SimulateResponse struct {
	Decision   string `json:"decision"`
	MatchedBy  string `json:"matched_by"`
	PolicyType string `json:"policy_type"`
	Details    string `json:"details"`
}

// CreateAccessKeyResult is returned when creating an access key
type CreateAccessKeyResult struct {
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"` // Only shown once
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

// MakeARN creates a Nexus ARN
func MakeARN(service, resource string) string {
	return ARNPrefix + service + ":::" + resource
}

// MakeUserARN creates a user ARN
func MakeUserARN(userName string) string {
	return MakeARN(ServiceIAM, "user/"+userName)
}

// MakeRoleARN creates a role ARN
func MakeRoleARN(roleName string) string {
	return MakeARN(ServiceIAM, "role/"+roleName)
}

// MakePolicyARN creates a policy ARN
func MakePolicyARN(policyName string) string {
	return MakeARN(ServiceIAM, "policy/"+policyName)
}

// MakeBucketARN creates a bucket ARN
func MakeBucketARN(bucketName string) string {
	return MakeARN(ServiceS3, bucketName)
}

// MakeObjectARN creates an object ARN
func MakeObjectARN(bucketName, objectKey string) string {
	return MakeARN(ServiceS3, bucketName+"/"+objectKey)
}

// MakeVectorIndexARN creates a vector index ARN for a bucket
func MakeVectorIndexARN(bucketName string) string {
	return MakeARN(ServiceVector, "index/"+bucketName)
}

// MakeVectorDocARN creates a vector document ARN for an indexed object
func MakeVectorDocARN(bucketName, objectKey string) string {
	return MakeARN(ServiceVector, "index/"+bucketName+"/"+objectKey)
}

// IsVectorAction checks if an action is a vector service action
func IsVectorAction(action string) bool {
	return strings.HasPrefix(action, ServiceVector+":")
}

// Well-known vector service actions
const (
	VectorActionSearch  = "vector:Search"
	VectorActionIndex   = "vector:Index"
	VectorActionDelete  = "vector:Delete"
	VectorActionManage  = "vector:Manage"  // rebuild, config
	VectorActionAll     = "vector:*"
)
