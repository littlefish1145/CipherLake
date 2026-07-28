package gateway

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cipherlake/internal/iam"
)

// IAMAuthBridge bridges the new IAM system with the existing AuthHandler
type IAMAuthBridge struct {
	iamService iam.IAMServiceProvider
	auth       *AuthHandler
}

// NewIAMAuthBridge creates a new bridge between IAM and the existing auth system
func NewIAMAuthBridge(iamService iam.IAMServiceProvider, auth *AuthHandler) *IAMAuthBridge {
	return &IAMAuthBridge{
		iamService: iamService,
		auth:       auth,
	}
}

// AuthenticateWithIAM authenticates a request using the new IAM system
// Falls back to the old auth system if IAM lookup fails
func (b *IAMAuthBridge) AuthenticateWithIAM(r *http.Request) (*User, *iam.IAMUser, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		if b.auth.config.RequireAuth {
			return nil, nil, fmt.Errorf("authentication required")
		}
		return &User{
			ID:          "anonymous",
			Name:        "anonymous",
			Role:        "anonymous",
			Permissions: []string{"read"},
		}, nil, nil
	}

	authType := strings.SplitN(authHeader, " ", 2)
	if len(authType) != 2 {
		return nil, nil, fmt.Errorf("invalid authorization header format")
	}

	switch strings.ToLower(authType[0]) {
	case "basic":
		return b.validateBasicAuthIAM(authType[1], r)
	case "bearer":
		user, err := b.auth.validateJWT(authType[1])
		return user, nil, err
	case "aws4-hmac-sha256":
		return b.validateAWSSignatureIAM(r)
	default:
		return nil, nil, fmt.Errorf("unsupported authorization type: %s", authType[0])
	}
}

// validateBasicAuthIAM validates Basic Auth using IAM access keys
func (b *IAMAuthBridge) validateBasicAuthIAM(encoded string, r *http.Request) (*User, *iam.IAMUser, error) {
	decoded, err := decodeBasicAuth(encoded)
	if err != nil {
		return nil, nil, err
	}

	accessKey := decoded.Username
	secretKey := decoded.Password

	// Try IAM access key first (AKIA prefix)
	if strings.HasPrefix(accessKey, iam.AccessKeyIDPrefix) {
		iamUser, ak, err := b.iamService.GetUserByAccessKeyID(accessKey)
		if err == nil && ak != nil {
			// Decrypt the secret key and compare
			decryptedSecret, err := b.iamService.DecryptSecretKey(ak.SecretKeyEnc)
			if err == nil && subtle.ConstantTimeCompare([]byte(secretKey), []byte(decryptedSecret)) == 1 {
				return iamUserToLegacy(iamUser), iamUser, nil
			}
		}
		return nil, nil, fmt.Errorf("invalid credentials")
	}

	// Try temp credentials (ASIA prefix)
	if strings.HasPrefix(accessKey, "ASIA") {
		tempCred, err := b.iamService.GetTempCredentialByAccessKeyID(accessKey)
		if err == nil {
			if subtle.ConstantTimeCompare([]byte(secretKey), []byte(tempCred.SecretAccessKey)) == 1 {
				// Temp credential - return a synthetic user
				return &User{
					ID:          accessKey,
					Name:        "sts:" + accessKey,
					Role:        "user",
					Permissions: []string{"read", "write", "delete"},
				}, nil, nil
			}
		}
		return nil, nil, fmt.Errorf("invalid credentials")
	}

	// Fall back to old auth system
	user, err := b.auth.validateBasicAuth(encoded)
	return user, nil, err
}

// validateAWSSignatureIAM validates AWS SigV4 signature using IAM access keys
func (b *IAMAuthBridge) validateAWSSignatureIAM(r *http.Request) (*User, *iam.IAMUser, error) {
	authorization := r.Header.Get("Authorization")

	if authorization == "" && r.URL.Query().Get("X-Amz-Credential") != "" {
		return b.validatePresignedURLIAM(r)
	}

	if authorization != "" {
		if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256") {
			return nil, nil, fmt.Errorf("invalid AWS signature version")
		}

		accessKey := extractAWSAccessKey(authorization)
		if accessKey == "" {
			return nil, nil, fmt.Errorf("missing access key in AWS signature")
		}

		// Try IAM access key
		if strings.HasPrefix(accessKey, iam.AccessKeyIDPrefix) {
			iamUser, ak, err := b.iamService.GetUserByAccessKeyID(accessKey)
			if err == nil && ak != nil {
				secretKey, err := b.iamService.DecryptSecretKey(ak.SecretKeyEnc)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to decrypt secret key")
				}

				if err := verifySigV4Header(r, secretKey); err != nil {
					return nil, nil, fmt.Errorf("AWS signature verification failed")
				}

				return iamUserToLegacy(iamUser), iamUser, nil
			}
		}

		// Try temp credentials (ASIA prefix)
		if strings.HasPrefix(accessKey, "ASIA") {
			tempCred, err := b.iamService.GetTempCredentialByAccessKeyID(accessKey)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid temporary credentials")
			}

			// Verify session token
			sessionToken := r.Header.Get("X-Amz-Security-Token")
			if sessionToken == "" {
				sessionToken = r.URL.Query().Get("X-Amz-Security-Token")
			}
			if subtle.ConstantTimeCompare([]byte(sessionToken), []byte(tempCred.SessionToken)) != 1 {
				return nil, nil, fmt.Errorf("invalid session token")
			}

			if err := verifySigV4Header(r, tempCred.SecretAccessKey); err != nil {
				return nil, nil, fmt.Errorf("AWS signature verification failed")
			}

			return &User{
				ID:          accessKey,
				Name:        "sts:" + accessKey,
				Role:        "user",
				Permissions: []string{"read", "write", "delete"},
			}, nil, nil
		}

		// Fall back to old auth system
		user, err := b.auth.validateAWSSignature(r)
		return user, nil, err
	}

	// Presigned URL
	return b.validatePresignedURLIAM(r)
}

func (b *IAMAuthBridge) validatePresignedURLIAM(r *http.Request) (*User, *iam.IAMUser, error) {
	accessKey := r.URL.Query().Get("X-Amz-Credential")
	if accessKey == "" {
		return nil, nil, fmt.Errorf("missing authorization header or presigned URL parameters")
	}
	parts := strings.Split(accessKey, "/")
	if len(parts) > 0 {
		accessKey = parts[0]
	}

	// Try IAM
	if strings.HasPrefix(accessKey, iam.AccessKeyIDPrefix) {
		iamUser, ak, err := b.iamService.GetUserByAccessKeyID(accessKey)
		if err == nil && ak != nil {
			secretKey, err := b.iamService.DecryptSecretKey(ak.SecretKeyEnc)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to decrypt secret key")
			}

			if err := verifySigV4PresignedURL(r, secretKey); err != nil {
				return nil, nil, fmt.Errorf("presigned URL signature verification failed")
			}

			return iamUserToLegacy(iamUser), iamUser, nil
		}
	}

	if strings.HasPrefix(accessKey, "ASIA") {
		tempCred, err := b.iamService.GetTempCredentialByAccessKeyID(accessKey)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid temporary credentials")
		}

		sessionToken := r.URL.Query().Get("X-Amz-Security-Token")
		if subtle.ConstantTimeCompare([]byte(sessionToken), []byte(tempCred.SessionToken)) != 1 {
			return nil, nil, fmt.Errorf("invalid session token")
		}

		if err := verifySigV4PresignedURL(r, tempCred.SecretAccessKey); err != nil {
			return nil, nil, fmt.Errorf("presigned URL signature verification failed")
		}

		return &User{
			ID:          accessKey,
			Name:        "sts:" + accessKey,
			Role:        "user",
			Permissions: []string{"read", "write", "delete"},
		}, nil, nil
	}

	// Fall back to old auth
	user, err := b.auth.validateAWSSignature(r)
	return user, nil, err
}

// GetIAMUser retrieves an IAM user by name.
func (b *IAMAuthBridge) GetIAMUser(name string) (*iam.IAMUser, error) {
	if b == nil || b.iamService == nil {
		return nil, fmt.Errorf("IAM service not available")
	}
	return b.iamService.GetUser(name)
}

// CheckIAMAccess checks if an IAM user has access to perform an action
func (b *IAMAuthBridge) CheckIAMAccess(iamUser *iam.IAMUser, action, bucket, key string, r *http.Request) error {
	if iamUser == nil {
		return nil // No IAM user, fall back to legacy auth
	}

	// Admin user always has access
	if iamUser.Name == "admin" {
		return nil
	}

	// Build the resource ARN
	var resource string
	if key != "" {
		resource = iam.MakeObjectARN(bucket, key)
	} else {
		resource = iam.MakeBucketARN(bucket)
	}

	// Build evaluation context
	ctx := &iam.EvalContext{
		Principal:  iam.MakeUserARN(iamUser.Name),
		Action:     action,
		Resource:   resource,
		SourceIP:   extractClientIP(r),
		Time:       time.Now(),
		Conditions: make(map[string]string),
	}

	// Evaluate
	result := b.iamService.EvaluateAccess(ctx)
	if result.Decision == iam.DecisionAllow {
		return nil
	}

	return fmt.Errorf("access denied: %s on %s (policy: %s)", action, resource, result.MatchedBy)
}

// CheckTempCredentialAccess checks access for temporary STS credentials
func (b *IAMAuthBridge) CheckTempCredentialAccess(accessKeyID, action, bucket, key string, r *http.Request) error {
	// Get temp credential to find the associated role
	_, err := b.iamService.GetTempCredentialByAccessKeyID(accessKeyID)
	if err != nil {
		return fmt.Errorf("invalid temporary credentials")
	}

	// For now, temp credentials from GetSessionToken inherit the caller's permissions
	// Temp credentials from AssumeRole are evaluated against the role's policies
	// This is a simplified check - the full implementation would track the role ARN

	var resource string
	if key != "" {
		resource = iam.MakeObjectARN(bucket, key)
	} else {
		resource = iam.MakeBucketARN(bucket)
	}

	ctx := &iam.EvalContext{
		Principal: "sts:" + accessKeyID,
		Action:    action,
		Resource:  resource,
		SourceIP:  extractClientIP(r),
		Time:      time.Now(),
	}

	result := b.iamService.EvaluateAccess(ctx)
	if result.Decision == iam.DecisionAllow {
		return nil
	}

	return fmt.Errorf("access denied: %s on %s", action, resource)
}

// iamUserToLegacy converts an IAM user to the legacy User type
func iamUserToLegacy(iamUser *iam.IAMUser) *User {
	role := "user"
	if iamUser.Name == "admin" {
		role = "admin"
	}

	// Determine permissions from attached policies
	perms := []string{}
	for _, policyName := range iamUser.AttachedPolicies {
		// Check for well-known policies
		if policyName == "AdministratorAccess" {
			perms = []string{"read", "write", "delete", "admin"}
			break
		}
	}

	if len(perms) == 0 {
		// Default permissions
		perms = []string{"read", "write"}
	}

	return &User{
		ID:          iamUser.ID,
		Name:        iamUser.Name,
		Role:        role,
		Permissions: perms,
	}
}

// basicAuthCredentials holds decoded basic auth credentials
type basicAuthCredentials struct {
	Username string
	Password string
}

// decodeBasicAuth decodes a Basic Auth header value
func decodeBasicAuth(encoded string) (*basicAuthCredentials, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid basic auth encoding: %w", err)
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid basic auth format")
	}

	return &basicAuthCredentials{
		Username: parts[0],
		Password: parts[1],
	}, nil
}

// extractClientIP extracts the client IP from a request
func extractClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	// Check X-Real-IP
	xri := r.Header.Get("X-Real-IP")
	if xri != "" {
		return xri
	}
	// Fall back to RemoteAddr
	idx := strings.LastIndex(r.RemoteAddr, ":")
	if idx != -1 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}
