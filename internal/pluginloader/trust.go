package pluginloader

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

// Trust tiers. Unsigned plugins load at Tier 0; plugins signed by a trusted
// key load at Tier 1; plugins signed by a core key load at Tier 2.
const (
	TierUntrusted = 0
	TierTrusted   = 1
	TierCore      = 2
)

// TrustStore holds trusted public keys and verifies plugin signatures.
// It is safe for concurrent use by multiple goroutines.
type TrustStore struct {
	keys    map[string]TrustedKey
	pubKeys map[string]ed25519.PublicKey
}

// LoadTrustStore loads trusted keys from a JSON file. The file must contain
// an array of TrustedKey objects. If path is empty, an empty store is returned
// (all plugins load at Tier 0 unless the loader provides another path later).
func LoadTrustStore(path string) (*TrustStore, error) {
	ts := &TrustStore{
		keys:    make(map[string]TrustedKey),
		pubKeys: make(map[string]ed25519.PublicKey),
	}
	if path == "" {
		return ts, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ts, nil
		}
		return nil, fmt.Errorf("failed to read trusted keys %q: %w", path, err)
	}
	return ts.LoadKeys(raw)
}

// LoadKeys parses trusted keys from JSON bytes and replaces the store's
// current contents. It returns the store for chaining.
func (ts *TrustStore) LoadKeys(raw []byte) (*TrustStore, error) {
	var keys []TrustedKey
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &keys); err != nil {
			return nil, fmt.Errorf("failed to parse trusted_keys.json: %w", err)
		}
	}

	ts.keys = make(map[string]TrustedKey, len(keys))
	ts.pubKeys = make(map[string]ed25519.PublicKey, len(keys))
	for _, k := range keys {
		if k.KeyID == "" {
			return nil, errors.New("trusted key missing key_id")
		}
		pub, err := base64.StdEncoding.DecodeString(k.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("key %q: invalid base64 public key: %w", k.KeyID, err)
		}
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("key %q: ed25519 public key must be %d bytes, got %d", k.KeyID, ed25519.PublicKeySize, len(pub))
		}
		ts.keys[k.KeyID] = k
		ts.pubKeys[k.KeyID] = ed25519.PublicKey(pub)
	}
	return ts, nil
}

// SHA256 returns the hex-encoded SHA-256 of the trusted_keys.json file used
// to initialize the store. The loader prints this at startup for audit
// purposes (spec P2-1).
func (ts *TrustStore) SHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// FileSHA256 reads the trusted_keys.json file at path and returns its
// SHA-256. If the file does not exist, it returns an empty string.
func FileSHA256(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// MarshalTrustedKeysFile serializes a slice of trusted keys to the canonical
// JSON file format. Keys are sorted by key_id for deterministic output.
func MarshalTrustedKeysFile(keys []TrustedKey) ([]byte, error) {
	sorted := make([]TrustedKey, len(keys))
	copy(sorted, keys)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].KeyID < sorted[j].KeyID })
	return json.MarshalIndent(sorted, "", "  ")
}

// TrustDigest computes the digest signed by plugin signatures. It is the
// SHA-256 of the concatenation of the manifest JSON bytes and the WASM bytes.
// The manifest JSON must be the exact bytes installed (canonical formatting is
// not required because the signature is over the installed bytes).
func TrustDigest(manifestJSON, wasmBytes []byte) []byte {
	h := sha256.New()
	h.Write(manifestJSON)
	h.Write(wasmBytes)
	return h.Sum(nil)
}

// VerifyResult reports the outcome of a signature check.
type VerifyResult struct {
	Tier  int
	KeyID string
}

// VerifySignature checks that sig is a valid Ed25519 signature over
// TrustDigest(manifestJSON, wasmBytes) made by the key identified by keyID.
// It returns the trust tier implied by the signing key.
func (ts *TrustStore) VerifySignature(manifestJSON, wasmBytes, sig []byte, keyID string) (*VerifyResult, error) {
	if keyID == "" {
		return nil, errors.New("key_id is empty")
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("invalid ed25519 signature length: got %d, want %d", len(sig), ed25519.SignatureSize)
	}
	pub, ok := ts.pubKeys[keyID]
	if !ok {
		return nil, fmt.Errorf("key %q is not in trusted_keys.json", keyID)
	}
	digest := TrustDigest(manifestJSON, wasmBytes)
	if !ed25519.Verify(pub, digest, sig) {
		return nil, errors.New("signature verification failed")
	}

	k := ts.keys[keyID]
	tier := TierTrusted
	if k.IsCore {
		tier = TierCore
	}
	return &VerifyResult{Tier: tier, KeyID: keyID}, nil
}

// DetermineEffectiveTier computes the effective trust tier for a plugin install.
//
// Rules (spec §3.7):
//   - No signature → Tier 0 regardless of requested tier (unless Tier 2 was
//     requested, which is an error).
//   - Unknown signing key → treat as unsigned (silent downgrade to Tier 0).
//     This lets admins install a plugin signed by an untrusted key at Tier 0.
//   - Known key with invalid signature → error (possible tampering).
//   - Valid signature with non-core key → Tier 1.
//   - Valid signature with core key → Tier 2.
//   - Requested Tier 2 with non-core signature → error (not a silent downgrade).
func (ts *TrustStore) DetermineEffectiveTier(requestedTier int, manifestJSON, wasmBytes, sig []byte, keyID string) (int, string, error) {
	if requestedTier < TierUntrusted || requestedTier > TierCore {
		return TierUntrusted, "", fmt.Errorf("invalid trust tier requested: %d", requestedTier)
	}

	// No signature provided.
	if len(sig) == 0 || keyID == "" {
		if requestedTier == TierCore {
			return TierUntrusted, "", errors.New("Tier 2 plugin requires a core signature")
		}
		return TierUntrusted, "", nil
	}

	// Unknown signing key: the plugin is signed, but not by anyone we trust.
	// Allow silent downgrade to Tier 0 so it can still be installed unsigned.
	if _, ok := ts.pubKeys[keyID]; !ok {
		if requestedTier == TierCore {
			return TierUntrusted, "", fmt.Errorf("Tier 2 plugin signed by unknown key %q", keyID)
		}
		return TierUntrusted, "", nil
	}

	res, err := ts.VerifySignature(manifestJSON, wasmBytes, sig, keyID)
	if err != nil {
		return TierUntrusted, "", err
	}

	// Signature valid. Honor requested tier up to the signing key's tier.
	if requestedTier == TierCore && res.Tier < TierCore {
		return TierUntrusted, "", fmt.Errorf("plugin requested Tier 2 but signed by non-core key %q", res.KeyID)
	}
	if requestedTier > res.Tier {
		return res.Tier, res.KeyID, nil
	}
	return requestedTier, res.KeyID, nil
}

// AddKey adds a trusted key to the in-memory store.
func (ts *TrustStore) AddKey(k TrustedKey) error {
	if k.KeyID == "" {
		return errors.New("trusted key missing key_id")
	}
	pub, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil {
		return fmt.Errorf("key %q: invalid base64 public key: %w", k.KeyID, err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("key %q: ed25519 public key must be %d bytes, got %d", k.KeyID, ed25519.PublicKeySize, len(pub))
	}
	ts.keys[k.KeyID] = k
	ts.pubKeys[k.KeyID] = ed25519.PublicKey(pub)
	return nil
}

// RemoveKey deletes a trusted key from the in-memory store. Returns true
// if the key existed.
func (ts *TrustStore) RemoveKey(keyID string) bool {
	_, existed := ts.keys[keyID]
	delete(ts.keys, keyID)
	delete(ts.pubKeys, keyID)
	return existed
}

// Keys returns a copy of all loaded trusted keys, sorted by key_id.
func (ts *TrustStore) Keys() []TrustedKey {
	keys := make([]TrustedKey, 0, len(ts.keys))
	for _, k := range ts.keys {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].KeyID < keys[j].KeyID })
	return keys
}

// KeyIDs returns the IDs of all loaded trusted keys, sorted.
func (ts *TrustStore) KeyIDs() []string {
	ids := make([]string, 0, len(ts.keys))
	for id := range ts.keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ErrInvalidSignature is returned when a plugin signature cannot be verified.
type ErrInvalidSignature struct {
	Reason string
}

func (e ErrInvalidSignature) Error() string {
	return fmt.Sprintf("invalid plugin signature: %s", e.Reason)
}

// IsErrInvalidSignature reports whether err is a signature verification failure.
func IsErrInvalidSignature(err error) bool {
	var e ErrInvalidSignature
	return errors.As(err, &e)
}
