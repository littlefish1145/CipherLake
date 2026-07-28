package keystore_service

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"cipherlake/internal/services"
)

// KeyStoreService stores encrypted DEKs
// No encryption keys - only stores encrypted DEKs
// Uses independent file storage for persistence
type KeyStoreService struct {
	mu       sync.RWMutex
	dataPath string               // Base path for key storage
	keys     map[string]*KeyEntry // In-memory cache
	auditLog *AuditLogger
}

// AuditLogger for keystore service
type AuditLogger struct {
	mu      sync.RWMutex
	entries []AuditEntry
	maxSize int
}

type AuditEntry struct {
	Timestamp time.Time
	Operation string
	KeyID     string
	Bucket    string
	ObjectKey string
	Result    string
}

// KeyEntry represents a stored key entry
type KeyEntry struct {
	KeyID        string    `json:"key_id"`
	Bucket       string    `json:"bucket"`
	ObjectKey    string    `json:"object_key"`
	EncryptedKey []byte    `json:"encrypted_key"`
	Algorithm    string    `json:"algorithm"`
	KeyVersion   int       `json:"key_version"`
	CreatedAt    time.Time `json:"created_at"`
	ObjectSize   int64     `json:"object_size,omitempty"`
}

// KeyStoreServiceConfig configuration
type KeyStoreServiceConfig struct {
	DataPath  string // Path to store key files
	AuditSize int    // Max audit entries
}

// NewKeyStoreService creates a new key store service
func NewKeyStoreService(cfg KeyStoreServiceConfig) (*KeyStoreService, error) {
	if cfg.DataPath == "" {
		cfg.DataPath = "./data/keystore"
	}

	// Create data directory
	if err := os.MkdirAll(cfg.DataPath, 0700); err != nil {
		return nil, fmt.Errorf("failed to create keystore directory: %w", err)
	}

	auditSize := cfg.AuditSize
	if auditSize <= 0 {
		auditSize = 10000
	}

	ks := &KeyStoreService{
		dataPath: cfg.DataPath,
		keys:     make(map[string]*KeyEntry),
		auditLog: &AuditLogger{
			entries: make([]AuditEntry, 0, auditSize),
			maxSize: auditSize,
		},
	}

	// Load existing keys
	if err := ks.loadAllKeys(); err != nil {
		zap.L().Warn("failed to load existing keys", zap.Error(err))
	}

	zap.L().Info("keystore initialized", zap.String("path", cfg.DataPath))
	return ks, nil
}

// StoreKey stores an encrypted DEK
func (k *KeyStoreService) StoreKey(bucket, objectKey string, encryptedDEK *services.EncryptedDEK, objectSize int64) (string, error) {
	if encryptedDEK.KeyID == "" {
		encryptedDEK.KeyID = generateKeyID()
	}

	entry := &KeyEntry{
		KeyID:        encryptedDEK.KeyID,
		Bucket:       bucket,
		ObjectKey:    objectKey,
		EncryptedKey: encryptedDEK.EncryptedKey,
		Algorithm:    encryptedDEK.Algorithm,
		KeyVersion:   encryptedDEK.KeyVersion,
		CreatedAt:    time.Now(),
		ObjectSize:   objectSize,
	}

	// Store in memory
	k.mu.Lock()
	keyIndex := k.makeKeyIndex(bucket, objectKey)
	k.keys[keyIndex] = entry
	k.mu.Unlock()

	// Persist to file
	if err := k.persistKey(entry); err != nil {
		k.mu.Lock()
		delete(k.keys, keyIndex)
		k.mu.Unlock()
		return "", fmt.Errorf("failed to persist key: %w", err)
	}

	// Log audit
	k.logAudit("store", entry.KeyID, bucket, objectKey, "success")

	zap.L().Info("key stored",
		zap.String("key_id", entry.KeyID),
		zap.String("bucket", bucket),
		zap.String("object_key", objectKey))

	return entry.KeyID, nil
}

// GetKey retrieves an encrypted DEK
func (k *KeyStoreService) GetKey(bucket, objectKey string) (*services.EncryptedDEK, error) {
	k.mu.RLock()
	keyIndex := k.makeKeyIndex(bucket, objectKey)
	entry, exists := k.keys[keyIndex]
	k.mu.RUnlock()

	if !exists {
		// Try to load from file
		entry, err := k.loadKey(bucket, objectKey)
		if err != nil {
			k.logAudit("get", "", bucket, objectKey, "not_found")
			return nil, fmt.Errorf("key not found for %s/%s", bucket, objectKey)
		}
		k.mu.Lock()
		k.keys[keyIndex] = entry
		k.mu.Unlock()
	}

	// Log audit
	k.logAudit("get", entry.KeyID, bucket, objectKey, "success")

	return &services.EncryptedDEK{
		EncryptedKey: entry.EncryptedKey,
		Algorithm:    entry.Algorithm,
		KeyID:        entry.KeyID,
		KeyVersion:   entry.KeyVersion,
	}, nil
}

// DeleteKey deletes an encrypted DEK
func (k *KeyStoreService) DeleteKey(bucket, objectKey string) error {
	keyIndex := k.makeKeyIndex(bucket, objectKey)

	k.mu.Lock()
	entry, exists := k.keys[keyIndex]
	if exists {
		delete(k.keys, keyIndex)
	}
	k.mu.Unlock()

	// Delete file
	if err := k.deleteKeyFile(bucket, objectKey); err != nil {
		k.logAudit("delete", "", bucket, objectKey, "error")
		return fmt.Errorf("failed to delete key file: %w", err)
	}

	// Log audit
	if entry != nil {
		k.logAudit("delete", entry.KeyID, bucket, objectKey, "success")
	} else {
		k.logAudit("delete", "", bucket, objectKey, "success")
	}

	zap.L().Info("key deleted",
		zap.String("bucket", bucket),
		zap.String("object_key", objectKey))

	return nil
}

// ListKeys lists keys for a bucket with optional prefix filter
func (k *KeyStoreService) ListKeys(bucket, prefix string, limit, offset int) ([]*KeyEntry, int, error) {
	if limit <= 0 {
		limit = 100
	}

	k.mu.RLock()
	defer k.mu.RUnlock()

	var results []*KeyEntry
	for _, entry := range k.keys {
		if entry.Bucket == bucket {
			if prefix == "" || hasPrefix(entry.ObjectKey, prefix) {
				results = append(results, entry)
			}
		}
	}

	total := len(results)

	// Apply offset and limit
	if offset >= total {
		return nil, total, nil
	}

	end := offset + limit
	if end > total {
		end = total
	}

	return results[offset:end], total, nil
}

// makeKeyIndex creates a unique index for a key
func (k *KeyStoreService) makeKeyIndex(bucket, objectKey string) string {
	return fmt.Sprintf("%s/%s", bucket, objectKey)
}

// persistKey persists a key entry to file
func (k *KeyStoreService) persistKey(entry *KeyEntry) error {
	// Create bucket directory
	bucketDir := filepath.Join(k.dataPath, entry.Bucket)
	if err := os.MkdirAll(bucketDir, 0700); err != nil {
		return fmt.Errorf("failed to create bucket directory: %w", err)
	}

	// Encode key entry
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal key entry: %w", err)
	}

	// Write to file (object key is base64 encoded to handle special characters)
	keyFile := filepath.Join(bucketDir, encodeObjectKey(entry.ObjectKey)+".key")

	// Write atomically
	tmpFile := keyFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write key file: %w", err)
	}

	return os.Rename(tmpFile, keyFile)
}

// loadKey loads a key entry from file
func (k *KeyStoreService) loadKey(bucket, objectKey string) (*KeyEntry, error) {
	keyFile := filepath.Join(k.dataPath, bucket, encodeObjectKey(objectKey)+".key")

	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}

	var entry KeyEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("failed to unmarshal key entry: %w", err)
	}

	return &entry, nil
}

// loadAllKeys loads all keys from disk
func (k *KeyStoreService) loadAllKeys() error {
	buckets, err := os.ReadDir(k.dataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read keystore directory: %w", err)
	}

	for _, bucket := range buckets {
		if !bucket.IsDir() {
			continue
		}

		bucketPath := filepath.Join(k.dataPath, bucket.Name())
		keyFiles, err := os.ReadDir(bucketPath)
		if err != nil {
			continue
		}

		for _, keyFile := range keyFiles {
			if filepath.Ext(keyFile.Name()) != ".key" {
				continue
			}

			data, err := os.ReadFile(filepath.Join(bucketPath, keyFile.Name()))
			if err != nil {
				continue
			}

			var entry KeyEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				continue
			}

			keyIndex := k.makeKeyIndex(entry.Bucket, entry.ObjectKey)
			k.mu.Lock()
			k.keys[keyIndex] = &entry
			k.mu.Unlock()
		}
	}

	zap.L().Info("loaded keys from disk", zap.Int("count", len(k.keys)))
	return nil
}

// deleteKeyFile deletes a key file
func (k *KeyStoreService) deleteKeyFile(bucket, objectKey string) error {
	keyFile := filepath.Join(k.dataPath, bucket, encodeObjectKey(objectKey)+".key")
	return os.Remove(keyFile)
}

// encodeObjectKey encodes object key for safe file naming
func encodeObjectKey(key string) string {
	return base64.URLEncoding.EncodeToString([]byte(key))
}

// hasPrefix checks if a string has a prefix
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// generateKeyID generates a unique key ID
func generateKeyID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand.Read failed")
	}
	return base64.URLEncoding.EncodeToString(b)
}

// logAudit logs an audit entry
func (k *KeyStoreService) logAudit(operation, keyID, bucket, objectKey, result string) {
	k.auditLog.mu.Lock()
	defer k.auditLog.mu.Unlock()

	entry := AuditEntry{
		Timestamp: time.Now(),
		Operation: operation,
		KeyID:     keyID,
		Bucket:    bucket,
		ObjectKey: objectKey,
		Result:    result,
	}

	k.auditLog.entries = append(k.auditLog.entries, entry)

	if len(k.auditLog.entries) > k.auditLog.maxSize {
		k.auditLog.entries = k.auditLog.entries[len(k.auditLog.entries)-k.auditLog.maxSize:]
	}
}

// GetStats returns statistics about the keystore
func (k *KeyStoreService) GetStats() map[string]interface{} {
	k.mu.RLock()
	defer k.mu.RUnlock()

	buckets := make(map[string]int)
	for _, entry := range k.keys {
		buckets[entry.Bucket]++
	}

	return map[string]interface{}{
		"total_keys":    len(k.keys),
		"buckets":       buckets,
		"data_path":     k.dataPath,
		"audit_entries": len(k.auditLog.entries),
	}
}

// Close cleans up the service
func (k *KeyStoreService) Close() error {
	// Flush any pending writes (none in current implementation)
	return nil
}
