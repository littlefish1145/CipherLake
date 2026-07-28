package iam

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"go.uber.org/zap"
)

// MasterKey manages the encryption key used to protect Secret Keys at rest
type MasterKey struct {
	key     []byte // 32 bytes for AES-256
	keyPath string
	loaded  bool
}

// NewMasterKey loads or creates a master key from the specified path
func NewMasterKey(keyPath string) (*MasterKey, error) {
	mk := &MasterKey{
		keyPath: keyPath,
	}

	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		// Generate new master key
		if err := mk.generate(); err != nil {
			return nil, fmt.Errorf("failed to generate master key: %w", err)
		}
		zap.L().Info("generated new master key", zap.String("path", keyPath))
	} else {
		// Load existing master key
		if err := mk.load(); err != nil {
			return nil, fmt.Errorf("failed to load master key: %w", err)
		}
		zap.L().Info("loaded existing master key", zap.String("path", keyPath))
	}

	return mk, nil
}

// generate creates a new 256-bit master key and saves it
func (mk *MasterKey) generate() error {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return fmt.Errorf("failed to generate random key: %w", err)
	}

	if err := os.WriteFile(mk.keyPath, key, 0600); err != nil {
		return fmt.Errorf("failed to write master key file: %w", err)
	}

	mk.key = key
	mk.loaded = true
	return nil
}

// load reads the master key from disk
func (mk *MasterKey) load() error {
	key, err := os.ReadFile(mk.keyPath)
	if err != nil {
		return fmt.Errorf("failed to read master key file: %w", err)
	}

	if len(key) != 32 {
		return fmt.Errorf("invalid master key size: expected 32 bytes, got %d", len(key))
	}

	mk.key = key
	mk.loaded = true
	return nil
}

// Encrypt encrypts plaintext using AES-256-GCM with the master key
func (mk *MasterKey) Encrypt(plaintext []byte) ([]byte, error) {
	if !mk.loaded {
		return nil, fmt.Errorf("master key not loaded")
	}

	block, err := aes.NewCipher(mk.key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Output: nonce || ciphertext || tag
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt decrypts AES-256-GCM encrypted data using the master key
func (mk *MasterKey) Decrypt(ciphertext []byte) ([]byte, error) {
	if !mk.loaded {
		return nil, fmt.Errorf("master key not loaded")
	}

	block, err := aes.NewCipher(mk.key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertextBody := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertextBody, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	return plaintext, nil
}

// DeriveKey derives a sub-key from the master key using a salt
// Used for deriving per-user encryption keys
func (mk *MasterKey) DeriveKey(salt []byte) ([]byte, error) {
	if !mk.loaded {
		return nil, fmt.Errorf("master key not loaded")
	}

	h := sha256.New()
	h.Write(mk.key)
	h.Write(salt)
	return h.Sum(nil), nil
}

// Zero clears the master key from memory
func (mk *MasterKey) Zero() {
	for i := range mk.key {
		mk.key[i] = 0
	}
	mk.loaded = false
}
