package gateway

import (
	"context"
	"fmt"
	"net/http"

	"nexus/internal/auth"
	"nexus/internal/iam"
)

// gatewayAuthProvider adapts the legacy AuthHandler and IAMAuthBridge to the
// unified auth.Provider interface. It is a migration shim: over time the legacy
// handler and IAM bridge should be collapsed into implementations of
// auth.Provider and auth.PermissionChecker.
type gatewayAuthProvider struct {
	authHandler *AuthHandler
	iamBridge   *IAMAuthBridge
}

func newGatewayAuthProvider(authHandler *AuthHandler, iamBridge *IAMAuthBridge) *gatewayAuthProvider {
	return &gatewayAuthProvider{
		authHandler: authHandler,
		iamBridge:   iamBridge,
	}
}

func (p *gatewayAuthProvider) Authenticate(r *http.Request) (*auth.Identity, error) {
	// Prefer IAM when an IAM bridge is configured and the request looks like an
	// AWS SigV4 or AKIA/ASIA credential.
	if p.iamBridge != nil && looksLikeIAMRequest(r) {
		legacyUser, iamUser, err := p.iamBridge.AuthenticateWithIAM(r)
		if err == nil && legacyUser != nil {
			return toAuthIdentity(legacyUser, iamUser), nil
		}
	}

	// Fall back to legacy authentication.
	legacyUser, err := p.authHandler.Authenticate(r)
	if err != nil {
		// When authentication is not required, mirror legacy RequireAuth behavior
		// and return an anonymous identity with broad permissions.
		if !p.authHandler.config.RequireAuth {
			return anonymousIdentity(p.authHandler.config.AnonymousRead), nil
		}
		return nil, err
	}
	return toAuthIdentity(legacyUser, nil), nil
}

func anonymousIdentity(anonymousRead bool) *auth.Identity {
	perms := []string{auth.ActionRead}
	if anonymousRead {
		perms = []string{auth.ActionRead, auth.ActionWrite, auth.ActionDelete}
	}
	return &auth.Identity{
		ID:          "anonymous",
		Name:        "anonymous",
		Role:        "anonymous",
		Permissions: perms,
		Source:      "legacy",
	}
}

func looksLikeIAMRequest(r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}
	return len(authHeader) >= 4 &&
		(authHeader[:4] == "AWS4" || containsAKIAOrASIA(authHeader))
}

func containsAKIAOrASIA(s string) bool {
	return containsPrefix(s, "AKIA") || containsPrefix(s, "ASIA")
}

func containsPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i <= len(s)-len(prefix); i++ {
		if s[i:i+len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func toAuthIdentity(u *User, iamUser *iam.IAMUser) *auth.Identity {
	if u == nil {
		return nil
	}
	id := &auth.Identity{
		ID:          u.ID,
		Name:        u.Name,
		Role:        u.Role,
		Permissions: u.Permissions,
		BucketPerms: u.BucketPermissions,
		Source:      "legacy",
	}
	if iamUser != nil {
		id.IsIAM = true
		id.IAMUserID = iamUser.ID
		id.Source = "iam"
	}
	return id
}

// Check implements auth.PermissionChecker by evaluating IAM policies for IAM
// identities and falling back to legacy permission checks for legacy identities.
func (p *gatewayAuthProvider) Check(ctx context.Context, identity *auth.Identity, action, bucket, key string, r *http.Request) error {
	if identity == nil {
		return fmt.Errorf("authentication required")
	}

	// Admin role always has access.
	if identity.Role == "admin" {
		return nil
	}

	// IAM identities are evaluated against IAM policies when possible.
	if identity.IsIAM && p.iamBridge != nil {
		iamUser, err := p.iamBridge.GetIAMUser(identity.Name)
		if err == nil && iamUser != nil {
			iamAction := mapActionToIAM(action)
			if err := p.iamBridge.CheckIAMAccess(iamUser, iamAction, bucket, key, r); err != nil {
				return err
			}
			return nil
		}
	}

	// Legacy permission check.
	return p.checkLegacyPermission(identity, action, bucket)
}

func (p *gatewayAuthProvider) checkLegacyPermission(identity *auth.Identity, action, bucket string) error {
	if bucket != "" && identity.BucketPerms != nil {
		var bucketPerms []string
		if bp, ok := identity.BucketPerms[bucket]; ok {
			bucketPerms = bp
		} else if bp, ok := identity.BucketPerms["*"]; ok {
			bucketPerms = bp
		}

		if len(bucketPerms) > 0 {
			for _, perm := range bucketPerms {
				if perm == action || perm == "*" {
					return nil
				}
			}
			return fmt.Errorf("permission denied: requires %s on bucket %s", action, bucket)
		}
	}

	for _, perm := range identity.Permissions {
		if perm == action || perm == "admin" {
			return nil
		}
	}

	return fmt.Errorf("permission denied: requires %s", action)
}

func mapActionToIAM(action string) string {
	switch action {
	case auth.ActionRead:
		return "s3:GetObject"
	case auth.ActionWrite:
		return "s3:PutObject"
	case auth.ActionDelete:
		return "s3:DeleteObject"
	case auth.ActionAdmin:
		return "s3:*"
	default:
		return action
	}
}
