package kms

import (
	"context"
)

// KMSClient defines the unified interface for Key Management Service operations.
// Implementations include local (ECIES), Vault Transit, and AWS KMS.
type KMSClient interface {
	// GenerateDataKey generates a new data encryption key.
	// Returns the plaintext key and the encrypted key.
	GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error)

	// DecryptDataKey decrypts an encrypted data key.
	DecryptDataKey(ctx context.Context, keyID string, encrypted []byte) (plaintext []byte, err error)

	// GetPublicKey retrieves the public key for a key ID.
	GetPublicKey(ctx context.Context, keyID string) (pub []byte, err error)

	// Close cleans up resources.
	Close() error
}
