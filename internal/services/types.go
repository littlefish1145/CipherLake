package services

import (
	"context"
	"crypto/ecdh"
)

// Shared types for crypto services

// ECDHEncryptedDEK represents DEK encrypted with ECDH-derived session key
type ECDHEncryptedDEK struct {
	Ciphertext         []byte
	Nonce              []byte
	EphemeralPublicKey []byte
}

// ECDHPublicKey represents an ECDH public key
type ECDHPublicKey struct {
	PublicKey []byte
	Curve     string
}

// EncryptedDEK represents an encrypted data encryption key
type EncryptedDEK struct {
	EncryptedKey []byte
	Algorithm    string
	KeyID        string
	KeyVersion   int
}

// KeyGenerator defines the interface for key generation operations
type KeyGenerator interface {
	GenerateDataKey(ctx context.Context, tokenID, userID, bucket, objectKey string, clientECDHPub *ECDHPublicKey) (*EncryptedDEK, *ECDHEncryptedDEK, *ECDHPublicKey, error)
	GetPublicKey() ([]byte, string, string)
	Close() error
}

// KeyUnwrapper defines the interface for key unwrapping operations
type KeyUnwrapper interface {
	UnwrapKey(ctx context.Context, tokenID, userID, bucket, objectKey string, encryptedDEK *EncryptedDEK, clientECDHPub *ECDHPublicKey) (*ECDHEncryptedDEK, *ECDHPublicKey, error)
	Close() error
}

// DataEncryptor defines the interface for data encryption operations
type DataEncryptor interface {
	Encrypt(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *ECDHPublicKey, ecdhEncryptedDEK *ECDHEncryptedDEK, plaintext []byte, algorithm string) ([]byte, []byte, []byte, error)
	Close() error
}

// DataDecryptor defines the interface for data decryption operations
type DataDecryptor interface {
	Decrypt(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *ECDHPublicKey, ecdhEncryptedDEK *ECDHEncryptedDEK, ciphertext []byte, nonce []byte, authTag []byte, algorithm string) ([]byte, error)
	Close() error
}

// KeyStorer defines the interface for key storage operations
type KeyStorer interface {
	StoreKey(bucket, objectKey string, encryptedDEK *EncryptedDEK, objectSize int64) (string, error)
	GetKey(bucket, objectKey string) (*EncryptedDEK, error)
	DeleteKey(bucket, objectKey string) error
	Close() error
}
