// Package auth defines the unified authentication and authorization interfaces
// used by the Nexus S3 gateway. Legacy local users and IAM users are both
// exposed through the same Identity abstraction.
package auth

import (
	"context"
	"net/http"
)

// Identity represents an authenticated caller, regardless of whether it came
// from the legacy user store or the IAM system.
type Identity struct {
	ID          string
	Name        string
	Role        string
	Permissions []string
	BucketPerms map[string][]string
	IsIAM       bool
	IAMUserID   string
	Source      string // e.g. "basic", "jwt", "sigv4", "anonymous"
}

// HasPermission reports whether the identity holds a global permission.
func (i *Identity) HasPermission(perm string) bool {
	for _, p := range i.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// HasBucketPermission reports whether the identity holds a permission on a bucket.
func (i *Identity) HasBucketPermission(bucket, perm string) bool {
	perms, ok := i.BucketPerms[bucket]
	if !ok {
		return false
	}
	for _, p := range perms {
		if p == perm {
			return true
		}
	}
	return false
}

// Provider authenticates an HTTP request and returns an Identity.
type Provider interface {
	// Authenticate extracts credentials from r and returns the authenticated identity.
	// If authentication is optional and no credentials are present, it may return
	// an anonymous identity with nil error.
	Authenticate(r *http.Request) (*Identity, error)
}

// PermissionChecker authorizes an already-authenticated identity for a specific
// action and resource.
type PermissionChecker interface {
	// Check returns nil if identity is allowed to perform action on the resource.
	Check(ctx context.Context, identity *Identity, action, bucket, key string, r *http.Request) error
}

// Action constants used across the gateway.
const (
	ActionRead   = "read"
	ActionWrite  = "write"
	ActionDelete = "delete"
	ActionAdmin  = "admin"
)
