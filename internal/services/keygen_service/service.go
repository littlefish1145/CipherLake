package keygen_service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/hkdf"

	"cipherlake/internal/services"
)

// KeyGenService generates data encryption keys (DEK)
// Uses ECIES (Elliptic Curve Integrated Encryption Scheme) for encrypting DEK
// Only has public key - cannot decrypt
type KeyGenService struct {
	mu                    sync.RWMutex
	encryptPublicKey      *ecdsa.PublicKey // Long-term public key for encrypting DEK
	encryptPublicKeyBytes []byte           // Serialized public key
	keyID                 string
	keyPath               string
	curve                 elliptic.Curve // P-256 for ECDH
	auditLog              *AuditLogger
}

// AuditLogger for keygen service
type AuditLogger struct {
	mu      sync.RWMutex
	entries []AuditEntry
	maxSize int
}

type AuditEntry struct {
	Timestamp time.Time
	Operation string
	KeyID     string
	TokenID   string
	UserID    string
	Bucket    string
	ObjectKey string
	Result    string
}

// KeyGenServiceConfig configuration
type KeyGenServiceConfig struct {
	KeyPath   string // Path to store/load public key
	KeyID     string // Optional key identifier
	CurveName string // Curve name: "P-256", "P-384", "P-521"
	AuditSize int    // Max audit entries
}

// NewKeyGenService creates a new key generation service
func NewKeyGenService(cfg KeyGenServiceConfig) (*KeyGenService, error) {
	// Select curve
	var curve elliptic.Curve
	switch cfg.CurveName {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		curve = elliptic.P256()
	}

	var pubKey *ecdsa.PublicKey
	var pubKeyBytes []byte
	var keyID string

	// Try to load existing public key
	if cfg.KeyPath != "" {
		pubKeyPath := cfg.KeyPath + ".pub"
		keyData, err := os.ReadFile(pubKeyPath)
		if err == nil {
			parsedKey, err := x509.ParsePKIXPublicKey(keyData)
			if err == nil {
				if ecdsaPub, ok := parsedKey.(*ecdsa.PublicKey); ok {
					pubKey = ecdsaPub
					pubKeyBytes = keyData
					zap.L().Info("loaded existing public key", zap.String("path", pubKeyPath))
				}
			}
		}
	}

	// If no public key loaded, generate a key pair
	if pubKey == nil {
		privKey, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("failed to generate key pair: %w", err)
		}
		pubKey = &privKey.PublicKey

		// Serialize public key
		pubKeyBytes, err = x509.MarshalPKIXPublicKey(pubKey)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal public key: %w", err)
		}

		// Save keys if path provided
		if cfg.KeyPath != "" {
			pubKeyPath := cfg.KeyPath + ".pub"
			dir := filepath.Dir(pubKeyPath)
			if err := os.MkdirAll(dir, 0700); err != nil {
				return nil, fmt.Errorf("failed to create key directory: %w", err)
			}
			if err := os.WriteFile(pubKeyPath, pubKeyBytes, 0644); err != nil {
				return nil, fmt.Errorf("failed to save public key: %w", err)
			}

			// Save private key for KeyUnwrapService
			privKeyBytes, err := x509.MarshalECPrivateKey(privKey)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal private key: %w", err)
			}
			privKeyPath := cfg.KeyPath + ".priv"
			if err := os.WriteFile(privKeyPath, privKeyBytes, 0600); err != nil {
				return nil, fmt.Errorf("failed to save private key: %w", err)
			}

			zap.L().Info("generated and saved key pair",
				zap.String("pub_path", pubKeyPath),
				zap.String("priv_path", privKeyPath))
		}
	}

	// Generate key ID
	if cfg.KeyID != "" {
		keyID = cfg.KeyID
	} else {
		keyIDBytes := make([]byte, 16)
		rand.Read(keyIDBytes)
		keyID = base64.URLEncoding.EncodeToString(keyIDBytes)
	}

	auditSize := cfg.AuditSize
	if auditSize <= 0 {
		auditSize = 10000
	}

	return &KeyGenService{
		encryptPublicKey:      pubKey,
		encryptPublicKeyBytes: pubKeyBytes,
		keyID:                 keyID,
		keyPath:               cfg.KeyPath,
		curve:                 curve,
		auditLog: &AuditLogger{
			entries: make([]AuditEntry, 0, auditSize),
			maxSize: auditSize,
		},
	}, nil
}

// GenerateDataKey generates a new DEK and encrypts it
func (k *KeyGenService) GenerateDataKey(ctx context.Context, tokenID, userID, bucket, objectKey string, clientECDHPub *services.ECDHPublicKey) (*services.EncryptedDEK, *services.ECDHEncryptedDEK, *services.ECDHPublicKey, error) {
	// Generate random DEK (32 bytes for AES-256)
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate DEK: %w", err)
	}

	// Encrypt DEK with long-term public key (ECIES)
	encryptedDEK, err := k.encryptDEKWithPublicKey(dek)
	if err != nil {
		clearBytes(dek)
		return nil, nil, nil, fmt.Errorf("failed to encrypt DEK with public key: %w", err)
	}

	// Generate ephemeral ECDH key pair for session
	ephemeralPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		clearBytes(dek)
		return nil, nil, nil, fmt.Errorf("failed to generate ephemeral ECDH key: %w", err)
	}
	ephemeralPub := ephemeralPriv.PublicKey()

	// Parse client's ECDH public key
	clientPub, err := ecdh.P256().NewPublicKey(clientECDHPub.PublicKey)
	if err != nil {
		clearBytes(dek)
		return nil, nil, nil, fmt.Errorf("failed to parse client ECDH public key: %w", err)
	}

	// Derive shared secret using ECDH
	sharedSecret, err := ephemeralPriv.ECDH(clientPub)
	if err != nil {
		clearBytes(dek)
		return nil, nil, nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	// Derive AES key from shared secret using HKDF
	sessionKey := k.deriveSessionKey(sharedSecret, ephemeralPub.Bytes(), clientPub.Bytes())

	// Encrypt DEK with session key
	ecdhEncryptedDEK, err := k.encryptDEKWithSessionKey(dek, sessionKey)
	if err != nil {
		clearBytes(dek)
		clearBytes(sessionKey)
		clearBytes(sharedSecret)
		return nil, nil, nil, fmt.Errorf("failed to encrypt DEK with session key: %w", err)
	}

	// Clear sensitive data
	clearBytes(dek)
	clearBytes(sessionKey)
	clearBytes(sharedSecret)

	// Log audit
	k.logAudit("generate", encryptedDEK.KeyID, tokenID, userID, bucket, objectKey, "success")

	zap.L().Info("data key generated",
		zap.String("key_id", encryptedDEK.KeyID),
		zap.String("token_id", tokenID),
		zap.String("bucket", bucket))

	return encryptedDEK, ecdhEncryptedDEK, &services.ECDHPublicKey{
		PublicKey: ephemeralPub.Bytes(),
		Curve:     "P-256",
	}, nil
}

// GetPublicKey returns the service's public key
func (k *KeyGenService) GetPublicKey() ([]byte, string, string) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.encryptPublicKeyBytes, k.keyID, "ECIES-P256"
}

// encryptDEKWithPublicKey encrypts DEK using ECIES
func (k *KeyGenService) encryptDEKWithPublicKey(dek []byte) (*services.EncryptedDEK, error) {
	k.mu.RLock()
	pubKey := k.encryptPublicKey
	k.mu.RUnlock()

	// ECIES encryption:
	// 1. Generate ephemeral EC key pair
	ephemeralPriv, err := ecdsa.GenerateKey(k.curve, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ephemeral key: %w", err)
	}
	ephemeralPub := &ephemeralPriv.PublicKey

	// 2. Derive shared secret using ECDH (manual for ECDSA)
	sharedX, _ := k.curve.ScalarMult(pubKey.X, pubKey.Y, ephemeralPriv.D.Bytes())
	sharedSecret := sharedX.Bytes()

	// 3. Derive symmetric key from shared secret
	symKey := k.deriveSymmetricKey(sharedSecret, ephemeralPub)

	// 4. Encrypt DEK with symmetric key
	block, err := aes.NewCipher(symKey)
	if err != nil {
		clearBytes(symKey)
		clearBytes(sharedSecret)
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		clearBytes(symKey)
		clearBytes(sharedSecret)
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		clearBytes(symKey)
		clearBytes(sharedSecret)
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, dek, nil)

	// 5. Encode result: ephemeral_pub || nonce || ciphertext
	ephemeralPubBytes := elliptic.Marshal(k.curve, ephemeralPub.X, ephemeralPub.Y)
	encryptedKey := append(ephemeralPubBytes, nonce...)
	encryptedKey = append(encryptedKey, ciphertext...)

	// Clear sensitive data
	clearBytes(symKey)
	clearBytes(sharedSecret)

	return &services.EncryptedDEK{
		EncryptedKey: encryptedKey,
		Algorithm:    "ECIES-P256-AES-256-GCM",
		KeyID:        generateKeyID(),
		KeyVersion:   1,
	}, nil
}

// encryptDEKWithSessionKey encrypts DEK using session key from ECDH
func (k *KeyGenService) encryptDEKWithSessionKey(dek, sessionKey []byte) (*services.ECDHEncryptedDEK, error) {
	block, err := aes.NewCipher(sessionKey)
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

	ciphertext := gcm.Seal(nil, nonce, dek, nil)

	return &services.ECDHEncryptedDEK{
		Ciphertext: ciphertext,
		Nonce:      nonce,
	}, nil
}

// deriveSymmetricKey derives symmetric key from shared secret for ECIES
func (k *KeyGenService) deriveSymmetricKey(sharedSecret []byte, ephemeralPub *ecdsa.PublicKey) []byte {
	info := elliptic.Marshal(k.curve, ephemeralPub.X, ephemeralPub.Y)
	return hkdfDerive(sharedSecret, nil, info, 32)
}

// deriveSessionKey derives session key from ECDH shared secret
func (k *KeyGenService) deriveSessionKey(sharedSecret, serverPub, clientPub []byte) []byte {
	info := append(serverPub, clientPub...)
	return hkdfDerive(sharedSecret, nil, info, 32)
}

// hkdfDerive derives a key using HKDF
func hkdfDerive(secret, salt, info []byte, keyLen int) []byte {
	reader := hkdf.New(sha256.New, secret, salt, info)
	key := make([]byte, keyLen)
	io.ReadFull(reader, key)
	return key
}

// clearBytes clears a byte slice
func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// logAudit logs an audit entry
func (k *KeyGenService) logAudit(operation, keyID, tokenID, userID, bucket, objectKey, result string) {
	k.auditLog.mu.Lock()
	defer k.auditLog.mu.Unlock()

	entry := AuditEntry{
		Timestamp: time.Now(),
		Operation: operation,
		KeyID:     keyID,
		TokenID:   tokenID,
		UserID:    userID,
		Bucket:    bucket,
		ObjectKey: objectKey,
		Result:    result,
	}

	k.auditLog.entries = append(k.auditLog.entries, entry)

	if len(k.auditLog.entries) > k.auditLog.maxSize {
		k.auditLog.entries = k.auditLog.entries[len(k.auditLog.entries)-k.auditLog.maxSize:]
	}
}

// Close cleans up the service
func (k *KeyGenService) Close() error {
	// KeyGenService only has public key, no sensitive data to clear
	return nil
}

func generateKeyID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand.Read failed")
	}
	return base64.URLEncoding.EncodeToString(b)
}
