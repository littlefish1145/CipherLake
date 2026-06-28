package gateway

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"nexus/internal/auth"

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
	t.Setenv("NEXUS_USER_STORE", filepath.Join(tmpDir, "users.json"))

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
}

func TestMapActionToIAM(t *testing.T) {
	assert.Equal(t, "s3:GetObject", mapActionToIAM(auth.ActionRead))
	assert.Equal(t, "s3:PutObject", mapActionToIAM(auth.ActionWrite))
	assert.Equal(t, "s3:DeleteObject", mapActionToIAM(auth.ActionDelete))
	assert.Equal(t, "s3:*", mapActionToIAM(auth.ActionAdmin))
	assert.Equal(t, "vector:Search", mapActionToIAM("vector:Search"))
}
