package token_service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// TokenService handles delegation token issuance and validation
// Uses Ed25519 for signing - isolated from key operations
type TokenService struct {
	mu             sync.RWMutex
	signingPrivKey ed25519.PrivateKey
	signingPubKey  ed25519.PublicKey
	keyID          string
	keyPath        string
	issuedTokens   map[string]*TokenRecord // Audit trail
	auditLog       *AuditLogger
}

// TokenRecord stores metadata about issued tokens
type TokenRecord struct {
	TokenID   string
	TokenType TokenType
	UserID    string
	Bucket    string
	ObjectKey string
	IssuedAt  time.Time
	ExpiresAt time.Time
	Revoked   bool
}

// TokenType represents token type
type TokenType int

const (
	TokenTypeWrite  TokenType = 0
	TokenTypeRead   TokenType = 1
	TokenTypeDelete TokenType = 2
)

// AuditLogger for token service
type AuditLogger struct {
	mu      sync.RWMutex
	entries []AuditEntry
	maxSize int
}

type AuditEntry struct {
	Timestamp time.Time
	Operation string
	TokenID   string
	UserID    string
	Bucket    string
	ObjectKey string
	Result    string
}

// TokenServiceConfig configuration
type TokenServiceConfig struct {
	KeyPath   string // Path to store/load Ed25519 key
	KeyID     string // Optional key identifier
	AuditSize int    // Max audit entries
}

// NewTokenService creates a new token service
func NewTokenService(cfg TokenServiceConfig) (*TokenService, error) {
	var privKey ed25519.PrivateKey
	var pubKey ed25519.PublicKey
	var keyID string

	// Try to load existing key
	if cfg.KeyPath != "" {
		keyData, err := os.ReadFile(cfg.KeyPath)
		if err == nil && len(keyData) >= ed25519.PrivateKeySize {
			privKey = ed25519.PrivateKey(keyData[:ed25519.PrivateKeySize])
			pubKey = privKey.Public().(ed25519.PublicKey)
			zap.L().Info("loaded existing ed25519 signing key", zap.String("path", cfg.KeyPath))
		}
	}

	// Generate new key if not loaded
	if privKey == nil {
		var genErr error
		pubKey, privKey, genErr = ed25519.GenerateKey(rand.Reader)
		if genErr != nil {
			return nil, fmt.Errorf("failed to generate ed25519 key: %w", genErr)
		}

		// Save key if path provided
		if cfg.KeyPath != "" {
			dir := filepath.Dir(cfg.KeyPath)
			if err := os.MkdirAll(dir, 0700); err != nil {
				return nil, fmt.Errorf("failed to create key directory: %w", err)
			}
			if err := os.WriteFile(cfg.KeyPath, privKey, 0600); err != nil {
				return nil, fmt.Errorf("failed to save signing key: %w", err)
			}
			zap.L().Info("generated and saved new ed25519 signing key", zap.String("path", cfg.KeyPath))
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

	return &TokenService{
		signingPrivKey: privKey,
		signingPubKey:  pubKey,
		keyID:          keyID,
		keyPath:        cfg.KeyPath,
		issuedTokens:   make(map[string]*TokenRecord),
		auditLog: &AuditLogger{
			entries: make([]AuditEntry, 0, auditSize),
			maxSize: auditSize,
		},
	}, nil
}

// DelegationToken represents a signed delegation token
type DelegationToken struct {
	TokenID     string    `json:"token_id"`
	TokenType   TokenType `json:"token_type"`
	UserID      string    `json:"user_id"`
	Bucket      string    `json:"bucket"`
	ObjectKey   string    `json:"object_key,omitempty"`
	Expiry      time.Time `json:"expiry"`
	CreatedAt   time.Time `json:"created_at"`
	Operations  []string  `json:"operations"`
	ContentHash string    `json:"content_hash,omitempty"`
	Signature   []byte    `json:"signature,omitempty"`
}

// IssueWriteToken issues a token for write/encrypt operations
func (t *TokenService) IssueWriteToken(ctx context.Context, userID, bucket, objectKey string, ttlSeconds int64) (*DelegationToken, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 30
	}

	token := &DelegationToken{
		TokenID:    generateTokenID(),
		TokenType:  TokenTypeWrite,
		UserID:     userID,
		Bucket:     bucket,
		ObjectKey:  objectKey,
		Expiry:     time.Now().Add(time.Duration(ttlSeconds) * time.Second),
		CreatedAt:  time.Now(),
		Operations: []string{"encrypt"},
	}

	if err := t.signToken(token); err != nil {
		return nil, fmt.Errorf("failed to sign token: %w", err)
	}

	t.recordToken(token)
	t.logAudit("issue_write", token.TokenID, userID, bucket, objectKey, "success")

	zap.L().Info("token issued",
		zap.String("token_type", "write"),
		zap.String("token_id", token.TokenID),
		zap.String("user_id", userID),
		zap.String("bucket", bucket))

	return token, nil
}

// IssueReadToken issues a token for read/decrypt operations
func (t *TokenService) IssueReadToken(ctx context.Context, userID, bucket, objectKey, contentHash string, ttlSeconds int64) (*DelegationToken, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 30
	}

	token := &DelegationToken{
		TokenID:     generateTokenID(),
		TokenType:   TokenTypeRead,
		UserID:      userID,
		Bucket:      bucket,
		ObjectKey:   objectKey,
		ContentHash: contentHash,
		Expiry:      time.Now().Add(time.Duration(ttlSeconds) * time.Second),
		CreatedAt:   time.Now(),
		Operations:  []string{"decrypt"},
	}

	if err := t.signToken(token); err != nil {
		return nil, fmt.Errorf("failed to sign token: %w", err)
	}

	t.recordToken(token)
	t.logAudit("issue_read", token.TokenID, userID, bucket, objectKey, "success")

	zap.L().Info("token issued",
		zap.String("token_type", "read"),
		zap.String("token_id", token.TokenID),
		zap.String("user_id", userID),
		zap.String("bucket", bucket))

	return token, nil
}

// IssueDeleteToken issues a token for delete operations
func (t *TokenService) IssueDeleteToken(ctx context.Context, userID, bucket, objectKey string, ttlSeconds int64) (*DelegationToken, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 30
	}

	token := &DelegationToken{
		TokenID:    generateTokenID(),
		TokenType:  TokenTypeDelete,
		UserID:     userID,
		Bucket:     bucket,
		ObjectKey:  objectKey,
		Expiry:     time.Now().Add(time.Duration(ttlSeconds) * time.Second),
		CreatedAt:  time.Now(),
		Operations: []string{"delete"},
	}

	if err := t.signToken(token); err != nil {
		return nil, fmt.Errorf("failed to sign token: %w", err)
	}

	t.recordToken(token)
	t.logAudit("issue_delete", token.TokenID, userID, bucket, objectKey, "success")

	zap.L().Info("token issued",
		zap.String("token_type", "delete"),
		zap.String("token_id", token.TokenID),
		zap.String("user_id", userID),
		zap.String("bucket", bucket))

	return token, nil
}

// ValidateToken validates a token's signature and expiry
func (t *TokenService) ValidateToken(ctx context.Context, token *DelegationToken, expectedType TokenType) error {
	// Check expiry
	if time.Now().After(token.Expiry) {
		t.logAudit("validate", token.TokenID, token.UserID, token.Bucket, token.ObjectKey, "expired")
		return fmt.Errorf("token expired")
	}

	// Verify signature
	if err := t.verifyToken(token); err != nil {
		t.logAudit("validate", token.TokenID, token.UserID, token.Bucket, token.ObjectKey, "invalid_signature")
		return fmt.Errorf("invalid signature: %w", err)
	}

	// Check token type
	if token.TokenType != expectedType {
		t.logAudit("validate", token.TokenID, token.UserID, token.Bucket, token.ObjectKey, "wrong_type")
		return fmt.Errorf("wrong token type: expected %d, got %d", expectedType, token.TokenType)
	}

	// Check if revoked
	t.mu.RLock()
	record, exists := t.issuedTokens[token.TokenID]
	t.mu.RUnlock()

	if exists && record.Revoked {
		t.logAudit("validate", token.TokenID, token.UserID, token.Bucket, token.ObjectKey, "revoked")
		return fmt.Errorf("token revoked")
	}

	t.logAudit("validate", token.TokenID, token.UserID, token.Bucket, token.ObjectKey, "valid")
	return nil
}

// GetPublicKey returns the service's public key for token verification
func (t *TokenService) GetPublicKey() (ed25519.PublicKey, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.signingPubKey, t.keyID
}

// RevokeToken revokes a previously issued token
func (t *TokenService) RevokeToken(ctx context.Context, tokenID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	record, exists := t.issuedTokens[tokenID]
	if !exists {
		return fmt.Errorf("token not found")
	}

	record.Revoked = true
	t.logAudit("revoke", tokenID, record.UserID, record.Bucket, record.ObjectKey, "success")

	zap.L().Info("token revoked", zap.String("token_id", tokenID))
	return nil
}

// signToken signs a token with Ed25519
func (t *TokenService) signToken(token *DelegationToken) error {
	// Clone token without signature for signing
	clone := *token
	clone.Signature = nil

	data, err := json.Marshal(&clone)
	if err != nil {
		return fmt.Errorf("failed to marshal token: %w", err)
	}

	t.mu.RLock()
	privKey := t.signingPrivKey
	t.mu.RUnlock()

	sig := ed25519.Sign(privKey, data)
	token.Signature = sig
	return nil
}

// verifyToken verifies a token's signature
func (t *TokenService) verifyToken(token *DelegationToken) error {
	sig := token.Signature
	if sig == nil {
		return fmt.Errorf("token has no signature")
	}

	// Clone token without signature for verification
	clone := *token
	clone.Signature = nil

	data, err := json.Marshal(&clone)
	if err != nil {
		return fmt.Errorf("failed to marshal token: %w", err)
	}

	t.mu.RLock()
	pubKey := t.signingPubKey
	t.mu.RUnlock()

	if !ed25519.Verify(pubKey, data, sig) {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}

// recordToken stores token metadata for audit
func (t *TokenService) recordToken(token *DelegationToken) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.issuedTokens[token.TokenID] = &TokenRecord{
		TokenID:   token.TokenID,
		TokenType: token.TokenType,
		UserID:    token.UserID,
		Bucket:    token.Bucket,
		ObjectKey: token.ObjectKey,
		IssuedAt:  token.CreatedAt,
		ExpiresAt: token.Expiry,
		Revoked:   false,
	}
}

// logAudit logs an audit entry
func (t *TokenService) logAudit(operation, tokenID, userID, bucket, objectKey, result string) {
	t.auditLog.mu.Lock()
	defer t.auditLog.mu.Unlock()

	entry := AuditEntry{
		Timestamp: time.Now(),
		Operation: operation,
		TokenID:   tokenID,
		UserID:    userID,
		Bucket:    bucket,
		ObjectKey: objectKey,
		Result:    result,
	}

	t.auditLog.entries = append(t.auditLog.entries, entry)

	if len(t.auditLog.entries) > t.auditLog.maxSize {
		t.auditLog.entries = t.auditLog.entries[len(t.auditLog.entries)-t.auditLog.maxSize:]
	}
}

// GetAuditLog returns recent audit entries
func (t *TokenService) GetAuditLog(limit int) []AuditEntry {
	t.auditLog.mu.RLock()
	defer t.auditLog.mu.RUnlock()

	if limit <= 0 || limit > len(t.auditLog.entries) {
		limit = len(t.auditLog.entries)
	}

	entries := make([]AuditEntry, limit)
	copy(entries, t.auditLog.entries[len(t.auditLog.entries)-limit:])
	return entries
}

// Close cleans up the service
func (t *TokenService) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Clear sensitive data
	for i := range t.signingPrivKey {
		t.signingPrivKey[i] = 0
	}

	return nil
}

func generateTokenID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand.Read failed")
	}
	return base64.URLEncoding.EncodeToString(b)
}
