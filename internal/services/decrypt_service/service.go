package decrypt_service

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/hkdf"

	"cipherlake/internal/services"
)

// DecryptService decrypts data using DEK
// No persistent keys - uses DEK passed in request
// Single responsibility: data decryption only
type DecryptService struct {
	mu       sync.RWMutex
	auditLog *AuditLogger
}

// AuditLogger for decrypt service
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

// DecryptServiceConfig configuration
type DecryptServiceConfig struct {
	AuditSize int
}

// NewDecryptService creates a new decrypt service
func NewDecryptService(cfg DecryptServiceConfig) *DecryptService {
	auditSize := cfg.AuditSize
	if auditSize <= 0 {
		auditSize = 10000
	}

	return &DecryptService{
		auditLog: &AuditLogger{
			entries: make([]AuditEntry, 0, auditSize),
			maxSize: auditSize,
		},
	}
}

// Decrypt decrypts ciphertext using the DEK derived from ECDH session
func (d *DecryptService) Decrypt(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *services.ECDHPublicKey, ecdhEncryptedDEK *services.ECDHEncryptedDEK, ciphertext []byte, nonce []byte, authTag []byte, algorithm string) ([]byte, error) {
	// Derive session key from ECDH
	sessionKey, err := d.deriveSessionKey(clientECDHPriv, serviceECDHPub)
	if err != nil {
		return nil, fmt.Errorf("failed to derive session key: %w", err)
	}

	// Decrypt DEK using session key
	dek, err := d.decryptDEKWithSessionKey(ecdhEncryptedDEK, sessionKey)
	if err != nil {
		clearBytes(sessionKey)
		return nil, fmt.Errorf("failed to decrypt DEK: %w", err)
	}

	// Decrypt ciphertext with DEK
	plaintext, err := d.decryptData(dek, ciphertext, nonce, authTag, algorithm)
	if err != nil {
		clearBytes(sessionKey)
		clearBytes(dek)
		return nil, fmt.Errorf("failed to decrypt data: %w", err)
	}

	// Clear sensitive data
	clearBytes(sessionKey)
	clearBytes(dek)

	// Log audit
	d.logAudit("decrypt", "", "", int64(len(ciphertext)), "success")

	zap.L().Debug("data decrypted",
		zap.Int("ciphertext_size", len(ciphertext)),
		zap.Int("plaintext_size", len(plaintext)))

	return plaintext, nil
}

// DecryptStream decrypts a stream of data
func (d *DecryptService) DecryptStream(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *services.ECDHPublicKey, ecdhEncryptedDEK *services.ECDHEncryptedDEK, ciphertext io.Reader, nonce []byte, authTag []byte, algorithm string) (io.Reader, error) {
	// Read all ciphertext
	data, err := io.ReadAll(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to read ciphertext: %w", err)
	}

	plaintext, err := d.Decrypt(clientECDHPriv, serviceECDHPub, ecdhEncryptedDEK, data, nonce, authTag, algorithm)
	if err != nil {
		return nil, err
	}

	return io.NopCloser(bytes.NewReader(plaintext)), nil
}

// deriveSessionKey derives session key from ECDH
func (d *DecryptService) deriveSessionKey(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *services.ECDHPublicKey) ([]byte, error) {
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
	// Info order must match KeyUnwrapService: serverPub || clientPub
	info := append(servicePub.Bytes(), clientECDHPriv.PublicKey().Bytes()...)
	sessionKey := hkdfDerive(sharedSecret, nil, info, 32)

	clearBytes(sharedSecret)
	return sessionKey, nil
}

// decryptDEKWithSessionKey decrypts DEK using session key
func (d *DecryptService) decryptDEKWithSessionKey(ecdhEncryptedDEK *services.ECDHEncryptedDEK, sessionKey []byte) ([]byte, error) {
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

// decryptData decrypts data with DEK
func (d *DecryptService) decryptData(dek []byte, ciphertext []byte, nonce []byte, authTag []byte, algorithm string) ([]byte, error) {
	if algorithm == "" {
		algorithm = "AES-256-GCM"
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Combine ciphertext and auth tag for GCM
	fullCiphertext := append(ciphertext, authTag...)

	plaintext, err := gcm.Open(nil, nonce, fullCiphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt data: %w", err)
	}

	return plaintext, nil
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
func (d *DecryptService) logAudit(operation, keyID, sessionID string, size int64, result string) {
	d.auditLog.mu.Lock()
	defer d.auditLog.mu.Unlock()

	entry := AuditEntry{
		Timestamp: time.Now(),
		Operation: operation,
		KeyID:     keyID,
		SessionID: sessionID,
		Size:      size,
		Result:    result,
	}

	d.auditLog.entries = append(d.auditLog.entries, entry)

	if len(d.auditLog.entries) > d.auditLog.maxSize {
		d.auditLog.entries = d.auditLog.entries[len(d.auditLog.entries)-d.auditLog.maxSize:]
	}
}

// Close cleans up the service
func (d *DecryptService) Close() error {
	// No persistent keys to clear
	return nil
}
