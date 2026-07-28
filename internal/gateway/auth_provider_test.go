package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"cipherlake/internal/auth"
	"cipherlake/internal/iam"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGatewayAuthProvider_Authenticate_Anonymous(t *testing.T) {
	authHandler := NewAuthHandlerWithConfig(&AuthConfig{
		RequireAuth:   false,
		AnonymousRead: true,
	})
	provider := newGatewayAuthProvider(authHandler, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	identity, err := provider.Authenticate(req)
	require.NoError(t, err)
	require.NotNil(t, identity)
	assert.Equal(t, "anonymous", identity.ID)
	assert.Equal(t, "legacy", identity.Source)
	assert.True(t, identity.HasPermission(auth.ActionRead))
}

func TestGatewayAuthProvider_Authenticate_RequireAuth(t *testing.T) {
	authHandler := NewAuthHandlerWithConfig(&AuthConfig{
		RequireAuth: true,
	})
	provider := newGatewayAuthProvider(authHandler, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	identity, err := provider.Authenticate(req)
	assert.Error(t, err)
	assert.Nil(t, identity)
}

func TestGatewayAuthProvider_Authenticate_LegacyUser(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CIPHERLAKE_USER_STORE", filepath.Join(tmpDir, "users.json"))

	authHandler := NewAuthHandlerWithConfig(&AuthConfig{
		RequireAuth: true,
	})
	userName := "testuser_" + t.Name()
	userID := "user-" + t.Name()
	require.NoError(t, authHandler.AddUser(userID, userName, "testpass", "", "user", []string{auth.ActionRead, auth.ActionWrite}, nil))
	provider := newGatewayAuthProvider(authHandler, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth(userName, "testpass"))
	identity, err := provider.Authenticate(req)
	require.NoError(t, err)
	require.NotNil(t, identity)
	assert.Equal(t, "user", identity.Role)
	assert.Equal(t, userID, identity.ID)
	assert.True(t, identity.HasPermission(auth.ActionRead))
	assert.True(t, identity.HasPermission(auth.ActionWrite))
}

func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

func TestGatewayAuthProvider_Check_AdminAllowed(t *testing.T) {
	authHandler := NewAuthHandlerWithConfig(&AuthConfig{})
	provider := newGatewayAuthProvider(authHandler, nil)

	identity := &auth.Identity{Role: "admin"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	err := provider.Check(context.Background(), identity, auth.ActionWrite, "bucket", "key", req)
	assert.NoError(t, err)
}

func TestGatewayAuthProvider_Check_NilIdentity(t *testing.T) {
	authHandler := NewAuthHandlerWithConfig(&AuthConfig{})
	provider := newGatewayAuthProvider(authHandler, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	err := provider.Check(context.Background(), nil, auth.ActionRead, "bucket", "key", req)
	assert.Error(t, err)
}

func TestGatewayAuthProvider_Check_GlobalPermission(t *testing.T) {
	authHandler := NewAuthHandlerWithConfig(&AuthConfig{})
	provider := newGatewayAuthProvider(authHandler, nil)

	identity := &auth.Identity{
		ID:          "user",
		Role:        "user",
		Permissions: []string{auth.ActionRead},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	assert.NoError(t, provider.Check(context.Background(), identity, auth.ActionRead, "bucket", "key", req))
	assert.Error(t, provider.Check(context.Background(), identity, auth.ActionWrite, "bucket", "key", req))
}

func TestGatewayAuthProvider_Check_BucketPermission(t *testing.T) {
	authHandler := NewAuthHandlerWithConfig(&AuthConfig{})
	provider := newGatewayAuthProvider(authHandler, nil)

	identity := &auth.Identity{
		ID:   "user",
		Role: "user",
		BucketPerms: map[string][]string{
			"bucket-a": {auth.ActionRead},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	assert.NoError(t, provider.Check(context.Background(), identity, auth.ActionRead, "bucket-a", "key", req))
	assert.Error(t, provider.Check(context.Background(), identity, auth.ActionWrite, "bucket-a", "key", req))
	assert.Error(t, provider.Check(context.Background(), identity, auth.ActionRead, "bucket-b", "key", req))
}

func TestGatewayAuthProvider_Check_WildcardBucketPermission(t *testing.T) {
	authHandler := NewAuthHandlerWithConfig(&AuthConfig{})
	provider := newGatewayAuthProvider(authHandler, nil)

	identity := &auth.Identity{
		ID:   "user",
		Role: "user",
		BucketPerms: map[string][]string{
			"*": {auth.ActionWrite},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	assert.NoError(t, provider.Check(context.Background(), identity, auth.ActionWrite, "any-bucket", "key", req))
}

func TestLooksLikeIAMRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/...")
	assert.True(t, looksLikeIAMRequest(req))

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Basic YWRtaW46YWRtaW4=")
	assert.False(t, looksLikeIAMRequest(req2))

	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.False(t, looksLikeIAMRequest(req3))

	req4 := httptest.NewRequest(http.MethodGet, "/?X-Amz-Credential=AKIAIOSFODNN7EXAMPLE/20260719/us-east-1/s3/aws4_request", nil)
	assert.True(t, looksLikeIAMRequest(req4))

	req5 := httptest.NewRequest(http.MethodGet, "/", nil)
	req5.Header.Set("Authorization", "Basic "+basicAuth("ASIAIOSFODNN7EXAMPLE", "secret"))
	assert.True(t, looksLikeIAMRequest(req5))
}

func TestMapActionToIAM(t *testing.T) {
	assert.Equal(t, "s3:GetObject", mapActionToIAM(auth.ActionRead))
	assert.Equal(t, "s3:PutObject", mapActionToIAM(auth.ActionWrite))
	assert.Equal(t, "s3:DeleteObject", mapActionToIAM(auth.ActionDelete))
	assert.Equal(t, "s3:*", mapActionToIAM(auth.ActionAdmin))
	assert.Equal(t, "vector:Search", mapActionToIAM("vector:Search"))
}

func TestIAMAuthBridge_BasicAuthTemporaryCredential(t *testing.T) {
	iamProvider := &fakeIAMProvider{
		tempCred: &iam.TemporaryCredential{
			AccessKeyID:     "ASIAIOSFODNN7EXAMPLE",
			SecretAccessKey: "temp-secret",
			SessionToken:    "session-token",
			Expiration:      time.Now().Add(time.Hour),
		},
	}
	authHandler := NewAuthHandlerWithConfig(&AuthConfig{RequireAuth: true})
	bridge := NewIAMAuthBridge(iamProvider, authHandler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	user, iamUser, err := bridge.AuthenticateWithIAM(reqWithBasicAuth(req, "ASIAIOSFODNN7EXAMPLE", "temp-secret"))

	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Nil(t, iamUser)
	assert.Equal(t, "sts:ASIAIOSFODNN7EXAMPLE", user.Name)
	assert.Equal(t, 0, iamProvider.userAccessKeyLookups, "ASIA credentials should not be looked up as IAM user access keys")
	assert.Equal(t, 1, iamProvider.tempCredentialLookups)

	identity := toAuthIdentity(user, nil)
	require.NotNil(t, identity)
	assert.Equal(t, "sts", identity.Source)
	assert.True(t, identity.IsIAM)
}

func reqWithBasicAuth(req *http.Request, username, password string) *http.Request {
	req.Header.Set("Authorization", "Basic "+basicAuth(username, password))
	return req
}

type fakeIAMProvider struct {
	userAccessKeyLookups  int
	tempCredentialLookups int
	tempCred              *iam.TemporaryCredential
}

func (f *fakeIAMProvider) GetUserByAccessKeyID(accessKeyID string) (*iam.IAMUser, *iam.AccessKey, error) {
	f.userAccessKeyLookups++
	return nil, nil, fmt.Errorf("not found")
}

func (f *fakeIAMProvider) DecryptSecretKey(encryptedSecret []byte) (string, error) {
	return "", fmt.Errorf("not implemented")
}

func (f *fakeIAMProvider) GetTempCredentialByAccessKeyID(accessKeyID string) (*iam.TemporaryCredential, error) {
	f.tempCredentialLookups++
	if f.tempCred != nil && f.tempCred.AccessKeyID == accessKeyID {
		return f.tempCred, nil
	}
	return nil, fmt.Errorf("not found")
}

func (f *fakeIAMProvider) GetUser(name string) (*iam.IAMUser, error) {
	return nil, fmt.Errorf("not found")
}

func (f *fakeIAMProvider) EvaluateAccess(ctx *iam.EvalContext) *iam.EvalResult {
	return &iam.EvalResult{Decision: iam.DecisionImplicitDeny}
}
