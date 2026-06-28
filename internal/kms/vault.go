package kms

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/hashicorp/vault/api"
	"go.uber.org/zap"
)

// VaultTransitKMS implements KMSClient using HashiCorp Vault Transit engine.
//
// It uses the following Vault Transit API endpoints:
//   - GenerateDataKey: POST /v1/transit/datakey/{key_name}
//   - DecryptDataKey:  POST /v1/transit/decrypt/{key_name}
//   - GetPublicKey:    GET  /v1/transit/keys/{key_name}
type VaultTransitKMS struct {
	client    *api.Client
	keyName   string
	maxRetries int
}

// VaultConfig holds configuration for the Vault Transit KMS.
type VaultConfig struct {
	// Address is the Vault server address (e.g. "https://vault:8200").
	Address string
	// TokenFile is the path to a file containing the Vault token.
	// Falls back to VAULT_TOKEN env var if empty.
	TokenFile string
	// TransitKey is the name of the Transit engine key.
	TransitKey string
	// MaxRetries is the maximum number of retry attempts for transient errors.
	MaxRetries int
}

// NewVaultTransitKMS creates a new Vault Transit KMS client.
func NewVaultTransitKMS(cfg VaultConfig) (*VaultTransitKMS, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("kms/vault: address is required")
	}
	if cfg.TransitKey == "" {
		return nil, fmt.Errorf("kms/vault: transit_key is required")
	}

	vaultConfig := api.DefaultConfig()
	vaultConfig.Address = cfg.Address

	client, err := api.NewClient(vaultConfig)
	if err != nil {
		return nil, fmt.Errorf("kms/vault: failed to create client: %w", err)
	}

	// Set token: from file, then env var
	token, err := loadVaultToken(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("kms/vault: failed to load token: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("kms/vault: no vault token available (set VAULT_TOKEN or configure vault_token_file)")
	}
	client.SetToken(token)

	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	return &VaultTransitKMS{
		client:     client,
		keyName:    cfg.TransitKey,
		maxRetries: maxRetries,
	}, nil
}

// loadVaultToken loads a Vault token from a file or the VAULT_TOKEN env var.
func loadVaultToken(tokenFile string) (string, error) {
	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("failed to read token file %s: %w", tokenFile, err)
		}
		// Trim whitespace/newlines
		token := string(data)
		for len(token) > 0 && (token[len(token)-1] == '\n' || token[len(token)-1] == '\r' || token[len(token)-1] == ' ') {
			token = token[:len(token)-1]
		}
		if token != "" {
			return token, nil
		}
	}
	// Fall back to environment variable
	return os.Getenv("VAULT_TOKEN"), nil
}

// GenerateDataKey generates a new data encryption key using Vault Transit datakey API.
// Returns the plaintext key and the encrypted (ciphertext) key.
func (v *VaultTransitKMS) GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error) {
	if length != 32 && length != 16 {
		return nil, nil, fmt.Errorf("kms/vault: unsupported key length %d (must be 16 or 32)", length)
	}

	bits := length * 8
	path := fmt.Sprintf("transit/datakey/%s", v.keyName)

	var result *api.Secret
	result, err = v.retry(func() (*api.Secret, error) {
		return v.client.Logical().Write(path, map[string]interface{}{
			"bits": bits,
		})
	})
	if err != nil {
		return nil, nil, fmt.Errorf("kms/vault: GenerateDataKey failed: %w", err)
	}
	if result == nil {
		return nil, nil, fmt.Errorf("kms/vault: GenerateDataKey returned empty response")
	}

	plaintextB64, _ := result.Data["plaintext"].(string)
	ciphertextB64, _ := result.Data["ciphertext"].(string)

	if plaintextB64 == "" || ciphertextB64 == "" {
		return nil, nil, fmt.Errorf("kms/vault: GenerateDataKey missing plaintext or ciphertext in response")
	}

	plaintext, err = base64.StdEncoding.DecodeString(plaintextB64)
	if err != nil {
		return nil, nil, fmt.Errorf("kms/vault: failed to decode plaintext: %w", err)
	}

	encrypted = []byte(ciphertextB64)

	return plaintext, encrypted, nil
}

// DecryptDataKey decrypts an encrypted data key using Vault Transit decrypt API.
func (v *VaultTransitKMS) DecryptDataKey(ctx context.Context, keyID string, encrypted []byte) (plaintext []byte, err error) {
	path := fmt.Sprintf("transit/decrypt/%s", v.keyName)
	ciphertext := string(encrypted)

	var result *api.Secret
	result, err = v.retry(func() (*api.Secret, error) {
		return v.client.Logical().Write(path, map[string]interface{}{
			"ciphertext": ciphertext,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("kms/vault: DecryptDataKey failed: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("kms/vault: DecryptDataKey returned empty response")
	}

	plaintextB64, _ := result.Data["plaintext"].(string)
	if plaintextB64 == "" {
		return nil, fmt.Errorf("kms/vault: DecryptDataKey missing plaintext in response")
	}

	plaintext, err = base64.StdEncoding.DecodeString(plaintextB64)
	if err != nil {
		return nil, fmt.Errorf("kms/vault: failed to decode plaintext: %w", err)
	}

	return plaintext, nil
}

// GetPublicKey retrieves the public key from Vault Transit keys API.
func (v *VaultTransitKMS) GetPublicKey(ctx context.Context, keyID string) (pub []byte, err error) {
	path := fmt.Sprintf("transit/keys/%s", v.keyName)

	var result *api.Secret
	result, err = v.retry(func() (*api.Secret, error) {
		return v.client.Logical().Read(path)
	})
	if err != nil {
		return nil, fmt.Errorf("kms/vault: GetPublicKey failed: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("kms/vault: GetPublicKey returned empty response")
	}

	keysRaw, ok := result.Data["keys"]
	if !ok {
		return nil, fmt.Errorf("kms/vault: GetPublicKey missing keys in response")
	}

	keys, ok := keysRaw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("kms/vault: GetPublicKey unexpected keys format")
	}

	// Get the latest version
	latestVersion := -1
	var latestKey map[string]interface{}
	for verStr, keyData := range keys {
		keyInfo, ok := keyData.(map[string]interface{})
		if !ok {
			continue
		}
		var ver int
		if _, err := fmt.Sscanf(verStr, "%d", &ver); err != nil {
			continue
		}
		if ver > latestVersion {
			latestVersion = ver
			latestKey = keyInfo
		}
	}

	if latestKey == nil {
		return nil, fmt.Errorf("kms/vault: GetPublicKey no key versions found")
	}

	pubKeyB64, _ := latestKey["public_key"].(string)
	if pubKeyB64 == "" {
		return nil, fmt.Errorf("kms/vault: GetPublicKey missing public_key in key info")
	}

	pub, err = base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		// The public key might be PEM-encoded, return as-is
		pub = []byte(pubKeyB64)
	}

	return pub, nil
}

// Close cleans up the Vault client resources.
func (v *VaultTransitKMS) Close() error {
	// The Vault API client doesn't require explicit cleanup
	return nil
}

// retry executes a Vault operation with exponential backoff on transient errors.
func (v *VaultTransitKMS) retry(fn func() (*api.Secret, error)) (*api.Secret, error) {
	var lastErr error
	for attempt := 0; attempt <= v.maxRetries; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}

		lastErr = err

		// Check if the error is retryable (seal state, network, rate limit)
		if !isRetryableVaultError(err) {
			return nil, err
		}

		if attempt < v.maxRetries {
			backoff := time.Duration(math.Pow(2, float64(attempt))) * 200 * time.Millisecond
			zap.L().Warn("kms/vault: transient error, retrying",
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
				zap.Error(err))
			time.Sleep(backoff)
		}
	}
	return nil, fmt.Errorf("kms/vault: max retries exceeded: %w", lastErr)
}

// isRetryableVaultError determines if a Vault error is transient and worth retrying.
func isRetryableVaultError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Vault seal errors
	if contains(errStr, "vault is sealed") || contains(errStr, "sealed") {
		return true
	}
	// Rate limiting
	if contains(errStr, "rate limit") || contains(errStr, "429") {
		return true
	}
	// Connection errors
	if contains(errStr, "connection refused") || contains(errStr, "timeout") || contains(errStr, "EOF") {
		return true
	}
	// Internal server errors (5xx)
	if contains(errStr, "500") || contains(errStr, "502") || contains(errStr, "503") {
		return true
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
