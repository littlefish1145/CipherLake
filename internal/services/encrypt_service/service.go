package encrypt_service

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/hkdf"

	"cipherlake/internal/services"
)

// EncryptService encrypts data using DEK
// No persistent keys - uses DEK passed in request
// Single responsibility: data encryption only
type EncryptService struct {
	mu       sync.RWMutex
	auditLog *AuditLogger
}

// AuditLogger for encrypt service
type AuditLogger struct {
	mu      sync.RWMutex
	entries []AuditEntry
	maxSize int
}

type AuditEntry struct {
	Timestamp time.Time
	Operation string
	KeyID     string
	SessionID string
	Size      int64
	Result    string
}

// EncryptServiceConfig configuration
type EncryptServiceConfig struct {
	AuditSize int
}

// NewEncryptService creates a new encrypt service
func NewEncryptService(cfg EncryptServiceConfig) *EncryptService {
	auditSize := cfg.AuditSize
	if auditSize <= 0 {
		auditSize = 10000
	}

	return &EncryptService{
		auditLog: &AuditLogger{
			entries: make([]AuditEntry, 0, auditSize),
			maxSize: auditSize,
		},
	}
}

// Encrypt encrypts plaintext using the DEK derived from ECDH session
func (e *EncryptService) Encrypt(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *services.ECDHPublicKey, ecdhEncryptedDEK *services.ECDHEncryptedDEK, plaintext []byte, algorithm string) ([]byte, []byte, []byte, error) {
	// Derive session key from ECDH
	sessionKey, err := e.deriveSessionKey(clientECDHPriv, serviceECDHPub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to derive session key: %w", err)
	}

	// Decrypt DEK using session key
	dek, err := e.decryptDEKWithSessionKey(ecdhEncryptedDEK, sessionKey)
	if err != nil {
		clearBytes(sessionKey)
		return nil, nil, nil, fmt.Errorf("failed to decrypt DEK: %w", err)
	}

	// Encrypt plaintext with DEK
	ciphertext, nonce, authTag, err := e.encryptData(dek, plaintext, algorithm)
	if err != nil {
		clearBytes(sessionKey)
		clearBytes(dek)
		return nil, nil, nil, fmt.Errorf("failed to encrypt data: %w", err)
	}

	// Clear sensitive data
	clearBytes(sessionKey)
	clearBytes(dek)

	// Log audit
	e.logAudit("encrypt", "", "", int64(len(plaintext)), "success")

	zap.L().Debug("data encrypted",
		zap.Int("plaintext_size", len(plaintext)),
		zap.Int("ciphertext_size", len(ciphertext)))

	return ciphertext, nonce, authTag, nil
}

// EncryptStream encrypts a stream of data
func (e *EncryptService) EncryptStream(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *services.ECDHPublicKey, ecdhEncryptedDEK *services.ECDHEncryptedDEK, plaintext io.Reader, algorithm string) (io.Reader, []byte, []byte, error) {
	// Read all plaintext (for simplicity, in production use chunked streaming)
	data, err := io.ReadAll(plaintext)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read plaintext: %w", err)
	}

	ciphertext, nonce, authTag, err := e.Encrypt(clientECDHPriv, serviceECDHPub, ecdhEncryptedDEK, data, algorithm)
	if err != nil {
		return nil, nil, nil, err
	}

	return bytes.NewReader(ciphertext), nonce, authTag, nil
}

// deriveSessionKey derives session key from ECDH
func (e *EncryptService) deriveSessionKey(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *services.ECDHPublicKey) ([]byte, error) {
	// Parse service's ECDH public key
	servicePub, err := ecdh.P256().NewPublicKey(serviceECDHPub.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse service ECDH public key: %w", err)
	}

	// Derive shared secret
	sharedSecret, err := clientECDHPriv.ECDH(servicePub)
	if err != nil {
		return nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	// Derive session key using HKDF
	// Info order must match KeyGenService: serverPub || clientPub
	info := append(servicePub.Bytes(), clientECDHPriv.PublicKey().Bytes()...)
	sessionKey := hkdfDerive(sharedSecret, nil, info, 32)

	clearBytes(sharedSecret)
	return sessionKey, nil
}

// decryptDEKWithSessionKey decrypts DEK using session key
func (e *EncryptService) decryptDEKWithSessionKey(ecdhEncryptedDEK *services.ECDHEncryptedDEK, sessionKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := ecdhEncryptedDEK.Nonce
	ciphertext := ecdhEncryptedDEK.Ciphertext

	dek, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt DEK: %w", err)
	}

	return dek, nil
}

// encryptData encrypts data with DEK
func (e *EncryptService) encryptData(dek []byte, plaintext []byte, algorithm string) ([]byte, []byte, []byte, error) {
	if algorithm == "" {
		algorithm = "AES-256-GCM"
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// GCM includes auth tag in ciphertext, extract it
	authTagLen := 16
	authTag := ciphertext[len(ciphertext)-authTagLen:]
	ciphertext = ciphertext[:len(ciphertext)-authTagLen]

	return ciphertext, nonce, authTag, nil
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
func (e *EncryptService) logAudit(operation, keyID, sessionID string, size int64, result string) {
	e.auditLog.mu.Lock()
	defer e.auditLog.mu.Unlock()

	entry := AuditEntry{
		Timestamp: time.Now(),
		Operation: operation,
		KeyID:     keyID,
		SessionID: sessionID,
		Size:      size,
		Result:    result,
	}

	e.auditLog.entries = append(e.auditLog.entries, entry)

	if len(e.auditLog.entries) > e.auditLog.maxSize {
		e.auditLog.entries = e.auditLog.entries[len(e.auditLog.entries)-e.auditLog.maxSize:]
	}
}

// Close cleans up the service
func (e *EncryptService) Close() error {
	// No persistent keys to clear
	return nil
}
