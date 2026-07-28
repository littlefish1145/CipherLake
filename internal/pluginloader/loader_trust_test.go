package pluginloader

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"cipherlake/internal/config"

	"github.com/stretchr/testify/require"
)

func minimalValidWASM(t *testing.T) []byte {
	t.Helper()
	return mustHexDecode(t, minimalWASMHex)
}

func TestLoader_InstallPlugin_SignatureVerification(t *testing.T) {
	keyID, pub, priv := generateTestKey(t, false)
	trustedPath := writeTrustedKeysFile(t, []TrustedKey{
		{KeyID: keyID, PublicKey: base64.StdEncoding.EncodeToString(pub)},
	})

	loader := newTestLoaderWithTrustPath(t, trustedPath)
	defer loader.Shutdown()

	manifest := &Manifest{
		ManifestVersion:      SupportedManifestVersion,
		APIVersion:           HostAPICompatRange,
		Name:                 "signed-demo",
		Version:              "1.0.0",
		TrustTierRequested:   TierTrusted,
		Capabilities:         []string{"http:route", "state:kv"},
		Routes:               []ManifestRoute{{Prefix: "/demo/*", Handler: "on_request"}},
	}
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	wasm := minimalValidWASM(t)

	digest := TrustDigest(manifestJSON, wasm)
	validSig := ed25519.Sign(priv, digest)

	t.Run("valid signature loads at requested tier", func(t *testing.T) {
		require.NoError(t, loader.InstallPlugin(context.Background(), manifest, wasm, validSig, keyID))
		holder := loader.plugins[manifest.Name]
		require.NotNil(t, holder)
		require.Equal(t, TierTrusted, holder.pool.trustTier)
	})

	t.Run("tampered wasm rejected", func(t *testing.T) {
		manifest.Name = "tampered-demo"
		manifest.Routes = []ManifestRoute{{Prefix: "/tampered/*", Handler: "on_request"}}
		manifestJSON, err := json.Marshal(manifest)
		require.NoError(t, err)
		digest := TrustDigest(manifestJSON, wasm)
		validSig := ed25519.Sign(priv, digest)
		tampered := append(wasm, 0x00)
		err = loader.InstallPlugin(context.Background(), manifest, tampered, validSig, keyID)
		require.Error(t, err)
		require.True(t, IsErrInvalidSignature(err))
	})

	t.Run("unknown key downgrades to tier 0", func(t *testing.T) {
		manifest.Name = "unknown-key-demo"
		manifest.Routes = []ManifestRoute{{Prefix: "/unknown/*", Handler: "on_request"}}
		manifestJSON, err := json.Marshal(manifest)
		require.NoError(t, err)
		digest := TrustDigest(manifestJSON, wasm)
		validSig := ed25519.Sign(priv, digest)
		err = loader.InstallPlugin(context.Background(), manifest, wasm, validSig, "not-in-store")
		require.NoError(t, err)
		holder := loader.plugins[manifest.Name]
		require.NotNil(t, holder)
		require.Equal(t, TierUntrusted, holder.pool.trustTier)
	})
}

func TestLoader_InstallPlugin_Tier2RequiresCore(t *testing.T) {
	keyID, pub, priv := generateTestKey(t, false)
	trustedPath := writeTrustedKeysFile(t, []TrustedKey{
		{KeyID: keyID, PublicKey: base64.StdEncoding.EncodeToString(pub)},
	})

	loader := newTestLoaderWithTrustPath(t, trustedPath)
	defer loader.Shutdown()

	manifest := &Manifest{
		ManifestVersion:      SupportedManifestVersion,
		APIVersion:           HostAPICompatRange,
		Name:                 "tier2-demo",
		Version:              "1.0.0",
		TrustTierRequested:   TierCore,
		Capabilities:         []string{"http:route", "iam:policy:evaluate"},
		Routes:               []ManifestRoute{{Prefix: "/admin/*", Handler: "on_request"}},
	}
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	wasm := minimalValidWASM(t)
	digest := TrustDigest(manifestJSON, wasm)
	validSig := ed25519.Sign(priv, digest)

	err = loader.InstallPlugin(context.Background(), manifest, wasm, validSig, keyID)
	require.Error(t, err)
	require.True(t, IsErrInvalidSignature(err))
}

func TestLoader_AddTrustedKey(t *testing.T) {
	_, pub, _ := generateTestKey(t, false)
	trustedPath := writeTrustedKeysFile(t, []TrustedKey{})
	loader := newTestLoaderWithTrustPath(t, trustedPath)
	defer loader.Shutdown()

	pubB64 := base64.StdEncoding.EncodeToString(pub)
	err := loader.AddTrustedKey(context.Background(), "new-key", pubB64, true, "admin")
	require.NoError(t, err)

	keys := loader.ListTrustedKeys()
	require.Len(t, keys, 1)
	require.Equal(t, "new-key", keys[0].KeyID)
	require.True(t, keys[0].IsCore)

	// File should be persisted.
	loaded, err := LoadTrustStore(trustedPath)
	require.NoError(t, err)
	require.Equal(t, []string{"new-key"}, loaded.KeyIDs())
}

func TestLoader_RemoveTrustedKey(t *testing.T) {
	keyID, pub, _ := generateTestKey(t, false)
	trustedPath := writeTrustedKeysFile(t, []TrustedKey{
		{KeyID: keyID, PublicKey: base64.StdEncoding.EncodeToString(pub)},
	})
	loader := newTestLoaderWithTrustPath(t, trustedPath)
	defer loader.Shutdown()

	err := loader.RemoveTrustedKey(context.Background(), keyID)
	require.NoError(t, err)
	require.Empty(t, loader.ListTrustedKeys())

	loaded, err := LoadTrustStore(trustedPath)
	require.NoError(t, err)
	require.Empty(t, loaded.KeyIDs())
}

func TestLoader_AddTrustedKey_InvalidPublicKey(t *testing.T) {
	trustedPath := writeTrustedKeysFile(t, []TrustedKey{})
	loader := newTestLoaderWithTrustPath(t, trustedPath)
	defer loader.Shutdown()

	err := loader.AddTrustedKey(context.Background(), "bad-key", "not-base64!!!", false, "")
	require.Error(t, err)
	require.Empty(t, loader.ListTrustedKeys())
}

func newTestLoaderWithTrustPath(t *testing.T, trustedKeysPath string) *Loader {
	t.Helper()
	cfg := &config.PluginLoaderConfig{
		CallTimeout:     "1s",
		TrustedKeysPath: trustedKeysPath,
		Raft: &config.RaftConfig{
			Enabled:    true,
			DataDir:    t.TempDir(),
			NodeID:     "loader-test",
			ListenAddr: "127.0.0.1:0",
		},
	}
	loader, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = loader.Shutdown() })

	// Wait for the single-node cluster to elect itself leader so that
	// RaftApply calls in tests do not fail with "not leader".
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if loader.raft.IsLeader() {
			return loader
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("loader raft node did not become leader within 5s")
	return nil
}
