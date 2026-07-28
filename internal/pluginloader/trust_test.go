package pluginloader

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func generateTestKey(t *testing.T, core bool) (keyID string, pub ed25519.PublicKey, priv ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	keyID = "test-key"
	if core {
		keyID = "core-key"
	}
	return keyID, pub, priv
}

func writeTrustedKeysFile(t *testing.T, keys []TrustedKey) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "trusted_keys.json")
	raw, err := MarshalTrustedKeysFile(keys)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0644))
	return path
}

func TestLoadTrustStore(t *testing.T) {
	_, pub, _ := generateTestKey(t, false)
	path := writeTrustedKeysFile(t, []TrustedKey{
		{KeyID: "k1", PublicKey: base64.StdEncoding.EncodeToString(pub), IsCore: true},
	})

	ts, err := LoadTrustStore(path)
	require.NoError(t, err)
	require.Equal(t, []string{"k1"}, ts.KeyIDs())
	require.NotNil(t, ts.pubKeys["k1"])
}

func TestLoadTrustStore_MissingFile(t *testing.T) {
	ts, err := LoadTrustStore(filepath.Join(t.TempDir(), "does-not-exist.json"))
	require.NoError(t, err)
	require.Empty(t, ts.KeyIDs())
}

func TestTrustStore_VerifySignature(t *testing.T) {
	keyID, pub, priv := generateTestKey(t, false)
	ts, err := LoadTrustStore("")
	require.NoError(t, err)
	_, err = ts.LoadKeys([]byte(`[{"key_id":"` + keyID + `","public_key":"` + base64.StdEncoding.EncodeToString(pub) + `"}]`))
	require.NoError(t, err)

	manifest := []byte(`{"name":"demo","version":"1.0.0"}`)
	wasm := []byte{0x00, 0x61, 0x73, 0x6d}
	digest := TrustDigest(manifest, wasm)
	sig := ed25519.Sign(priv, digest)

	res, err := ts.VerifySignature(manifest, wasm, sig, keyID)
	require.NoError(t, err)
	require.Equal(t, TierTrusted, res.Tier)
	require.Equal(t, keyID, res.KeyID)

	// Tampered wasm must fail.
	_, err = ts.VerifySignature(manifest, append(wasm, 0x01), sig, keyID)
	require.Error(t, err)

	// Wrong key id.
	_, err = ts.VerifySignature(manifest, wasm, sig, "unknown")
	require.Error(t, err)
}

func TestTrustStore_DetermineEffectiveTier(t *testing.T) {
	keyID, pub, priv := generateTestKey(t, false)
	ts, err := LoadTrustStore("")
	require.NoError(t, err)
	_, err = ts.LoadKeys([]byte(`[{"key_id":"` + keyID + `","public_key":"` + base64.StdEncoding.EncodeToString(pub) + `"}]`))
	require.NoError(t, err)

	manifest := []byte(`{"name":"demo","version":"1.0.0"}`)
	wasm := []byte{0x00, 0x61, 0x73, 0x6d}
	digest := TrustDigest(manifest, wasm)
	sig := ed25519.Sign(priv, digest)

	t.Run("no signature", func(t *testing.T) {
		tier, _, err := ts.DetermineEffectiveTier(TierTrusted, manifest, wasm, nil, "")
		require.NoError(t, err)
		require.Equal(t, TierUntrusted, tier)
	})

	t.Run("valid trusted signature", func(t *testing.T) {
		tier, kid, err := ts.DetermineEffectiveTier(TierTrusted, manifest, wasm, sig, keyID)
		require.NoError(t, err)
		require.Equal(t, TierTrusted, tier)
		require.Equal(t, keyID, kid)
	})

	t.Run("tier 2 requested without core key", func(t *testing.T) {
		_, _, err := ts.DetermineEffectiveTier(TierCore, manifest, wasm, sig, keyID)
		require.Error(t, err)
	})

	t.Run("invalid signature with known key is an error", func(t *testing.T) {
		badSig := make([]byte, ed25519.SignatureSize)
		_, _, err := ts.DetermineEffectiveTier(TierTrusted, manifest, wasm, badSig, keyID)
		require.Error(t, err)
	})
}

func TestTrustStore_CoreKey(t *testing.T) {
	keyID, pub, priv := generateTestKey(t, true)
	ts, err := LoadTrustStore("")
	require.NoError(t, err)
	_, err = ts.LoadKeys([]byte(`[{"key_id":"` + keyID + `","public_key":"` + base64.StdEncoding.EncodeToString(pub) + `","is_core":true}]`))
	require.NoError(t, err)

	manifest := []byte(`{"name":"core-plugin"}`)
	wasm := []byte{0x00}
	sig := ed25519.Sign(priv, TrustDigest(manifest, wasm))

	tier, _, err := ts.DetermineEffectiveTier(TierCore, manifest, wasm, sig, keyID)
	require.NoError(t, err)
	require.Equal(t, TierCore, tier)
}

func TestMarshalTrustedKeysFile(t *testing.T) {
	keys := []TrustedKey{
		{KeyID: "beta", PublicKey: "cHViYmV0YQ=="},
		{KeyID: "alpha", PublicKey: "YWxwaGE=", IsCore: true},
	}
	raw, err := MarshalTrustedKeysFile(keys)
	require.NoError(t, err)
	var out []TrustedKey
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "alpha", out[0].KeyID)
	require.Equal(t, "beta", out[1].KeyID)
}
