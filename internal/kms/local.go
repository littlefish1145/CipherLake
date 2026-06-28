package kms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/hkdf"
	"go.uber.org/zap"
)

// LocalKMS implements KMSClient using a local ECDSA key pair stored on disk.
// It uses ECIES (Elliptic Curve Integrated Encryption Scheme) for
// encrypting/decrypting data encryption keys.
type LocalKMS struct {
	mu          sync.RWMutex
	privateKey  *ecdsa.PrivateKey
	publicKey   *ecdsa.PublicKey
	pubKeyBytes []byte
	keyPath     string
	curve       elliptic.Curve
}

// LocalConfig holds configuration for the local KMS.
type LocalConfig struct {
	// KeyPath is the base path for key files (.pub and .priv suffixes).
	KeyPath string
}

// NewLocalKMS creates a new LocalKMS, loading or generating an ECDSA key pair.
func NewLocalKMS(cfg LocalConfig) (*LocalKMS, error) {
	if cfg.KeyPath == "" {
		cfg.KeyPath = "./data/keys/keygen"
	}

	curve := elliptic.P256()
	var privKey *ecdsa.PrivateKey
	var pubKeyBytes []byte

	// Try to load existing keys
	privKeyPath := cfg.KeyPath + ".priv"
	pubKeyPath := cfg.KeyPath + ".pub"

	// Load private key
	privData, err := os.ReadFile(privKeyPath)
	if err == nil {
		privKey, err = x509.ParseECPrivateKey(privData)
		if err != nil {
			zap.L().Warn("kms/local: failed to parse existing private key, generating new one", zap.Error(err))
			privKey = nil
		} else {
			zap.L().Info("kms/local: loaded existing private key", zap.String("path", privKeyPath))
		}
	}

	// If no private key, generate a new key pair
	if privKey == nil {
		privKey, err = ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("kms/local: failed to generate key pair: %w", err)
		}

		// Save keys
		dir := filepath.Dir(cfg.KeyPath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("kms/local: failed to create key directory: %w", err)
		}

		// Save private key
		privKeyBytes, err := x509.MarshalECPrivateKey(privKey)
		if err != nil {
			return nil, fmt.Errorf("kms/local: failed to marshal private key: %w", err)
		}
		if err := os.WriteFile(privKeyPath, privKeyBytes, 0600); err != nil {
			return nil, fmt.Errorf("kms/local: failed to save private key: %w", err)
		}

		// Save public key
		pubKeyBytes, err = x509.MarshalPKIXPublicKey(&privKey.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("kms/local: failed to marshal public key: %w", err)
		}
		if err := os.WriteFile(pubKeyPath, pubKeyBytes, 0644); err != nil {
			return nil, fmt.Errorf("kms/local: failed to save public key: %w", err)
		}

		zap.L().Info("kms/local: generated and saved key pair",
			zap.String("priv_path", privKeyPath),
			zap.String("pub_path", pubKeyPath))
	} else {
		// Load public key bytes
		pubKeyBytes, err = x509.MarshalPKIXPublicKey(&privKey.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("kms/local: failed to marshal public key: %w", err)
		}
	}

	return &LocalKMS{
		privateKey:  privKey,
		publicKey:   &privKey.PublicKey,
		pubKeyBytes: pubKeyBytes,
		keyPath:     cfg.KeyPath,
		curve:       curve,
	}, nil
}

// GenerateDataKey generates a new random DEK and encrypts it with ECIES
// using the local public key.
func (l *LocalKMS) GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error) {
	if length <= 0 {
		length = 32
	}

	// Generate random DEK
	dek := make([]byte, length)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, nil, fmt.Errorf("kms/local: failed to generate DEK: %w", err)
	}

	// Encrypt DEK with ECIES
	encrypted, err = l.encryptWithECIES(dek)
	if err != nil {
		clearBytesLocal(dek)
		return nil, nil, fmt.Errorf("kms/local: failed to encrypt DEK: %w", err)
	}

	return dek, encrypted, nil
}

// DecryptDataKey decrypts an ECIES-encrypted data key using the local private key.
func (l *LocalKMS) DecryptDataKey(ctx context.Context, keyID string, encrypted []byte) (plaintext []byte, err error) {
	plaintext, err = l.decryptWithECIES(encrypted)
	if err != nil {
		return nil, fmt.Errorf("kms/local: failed to decrypt DEK: %w", err)
	}
	return plaintext, nil
}

// GetPublicKey returns the ECDSA public key in PKIX format.
func (l *LocalKMS) GetPublicKey(ctx context.Context, keyID string) (pub []byte, err error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.pubKeyBytes == nil {
		return nil, fmt.Errorf("kms/local: public key not available")
	}

	// Return a copy
	pub = make([]byte, len(l.pubKeyBytes))
	copy(pub, l.pubKeyBytes)
	return pub, nil
}

// Close clears the private key from memory.
func (l *LocalKMS) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.privateKey != nil {
		// Clear private key bytes
		dBytes := l.privateKey.D.Bytes()
		for i := range dBytes {
			dBytes[i] = 0
		}
		l.privateKey = nil
	}
	return nil
}

// encryptWithECIES encrypts data using ECIES with the local public key.
// Format: ephemeral_pub (65 bytes for P-256 uncompressed) || nonce (12 bytes) || ciphertext+tag
func (l *LocalKMS) encryptWithECIES(plaintext []byte) ([]byte, error) {
	l.mu.RLock()
	pubKey := l.publicKey
	curve := l.curve
	l.mu.RUnlock()

	if pubKey == nil {
		return nil, fmt.Errorf("public key not available")
	}

	// Generate ephemeral EC key pair
	ephemeralPriv, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ephemeral key: %w", err)
	}

	// Derive shared secret using ECDH
	sharedX, _ := curve.ScalarMult(pubKey.X, pubKey.Y, ephemeralPriv.D.Bytes())
	sharedSecret := sharedX.Bytes()

	// Derive symmetric key from shared secret using HKDF
	ephemeralPubBytes := elliptic.Marshal(curve, ephemeralPriv.PublicKey.X, ephemeralPriv.PublicKey.Y)
	symKey := hkdfDeriveLocal(sharedSecret, nil, ephemeralPubBytes, 32)

	// Encrypt with AES-256-GCM
	block, err := aes.NewCipher(symKey)
	if err != nil {
		clearBytesLocal(symKey)
		clearBytesLocal(sharedSecret)
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		clearBytesLocal(symKey)
		clearBytesLocal(sharedSecret)
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		clearBytesLocal(symKey)
		clearBytesLocal(sharedSecret)
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Combine: ephemeral_pub || nonce || ciphertext
	result := append(ephemeralPubBytes, nonce...)
	result = append(result, ciphertext...)

	clearBytesLocal(symKey)
	clearBytesLocal(sharedSecret)

	return result, nil
}

// decryptWithECIES decrypts ECIES-encrypted data using the local private key.
func (l *LocalKMS) decryptWithECIES(encrypted []byte) ([]byte, error) {
	l.mu.RLock()
	privKey := l.privateKey
	curve := l.curve
	l.mu.RUnlock()

	if privKey == nil {
		return nil, fmt.Errorf("private key not available")
	}

	// Parse format: ephemeral_pub (65 bytes) || nonce (12 bytes) || ciphertext+tag
	pubKeyLen := 65 // P-256 uncompressed point
	if len(encrypted) < pubKeyLen {
		return nil, fmt.Errorf("invalid encrypted data: too short")
	}

	ephemeralPubBytes := encrypted[:pubKeyLen]
	ephemeralPubX, ephemeralPubY := elliptic.Unmarshal(curve, ephemeralPubBytes)
	if ephemeralPubX == nil {
		return nil, fmt.Errorf("failed to parse ephemeral public key")
	}

	// Derive shared secret using ECDH
	sharedX, _ := curve.ScalarMult(ephemeralPubX, ephemeralPubY, privKey.D.Bytes())
	sharedSecret := sharedX.Bytes()

	// Derive symmetric key from shared secret using HKDF
	symKey := hkdfDeriveLocal(sharedSecret, nil, ephemeralPubBytes, 32)

	// Parse nonce and ciphertext
	nonceLen := 12
	if len(encrypted) < pubKeyLen+nonceLen {
		clearBytesLocal(symKey)
		clearBytesLocal(sharedSecret)
		return nil, fmt.Errorf("invalid encrypted data: missing nonce")
	}

	nonce := encrypted[pubKeyLen : pubKeyLen+nonceLen]
	ciphertext := encrypted[pubKeyLen+nonceLen:]

	// Decrypt with AES-256-GCM
	block, err := aes.NewCipher(symKey)
	if err != nil {
		clearBytesLocal(symKey)
		clearBytesLocal(sharedSecret)
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		clearBytesLocal(symKey)
		clearBytesLocal(sharedSecret)
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		clearBytesLocal(symKey)
		clearBytesLocal(sharedSecret)
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}

	clearBytesLocal(symKey)
	clearBytesLocal(sharedSecret)

	return plaintext, nil
}

// hkdfDeriveLocal derives a key using HKDF-SHA256.
func hkdfDeriveLocal(secret, salt, info []byte, keyLen int) []byte {
	reader := hkdf.New(sha256.New, secret, salt, info)
	key := make([]byte, keyLen)
	io.ReadFull(reader, key)
	return key
}

// clearBytesLocal zeroes out a byte slice.
func clearBytesLocal(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
