package services

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"go.uber.org/zap"

	"nexus/internal/services/token_service"
)

// TokenIssuer defines the interface for token issuance used by the coordinator.
// Both the local TokenService and GRPCTokenService implement this interface.
type TokenIssuer interface {
	IssueWriteToken(ctx context.Context, userID, bucket, objectKey string, ttlSeconds int64) (*token_service.DelegationToken, error)
	IssueReadToken(ctx context.Context, userID, bucket, objectKey, contentHash string, ttlSeconds int64) (*token_service.DelegationToken, error)
	IssueDeleteToken(ctx context.Context, userID, bucket, objectKey string, ttlSeconds int64) (*token_service.DelegationToken, error)
	Close() error
}

// EncryptionCoordinator coordinates all crypto microservices
// Main service uses this to orchestrate encryption/decryption operations.
// Data encryption uses per-chunk key derivation streaming AES-256-GCM locally,
// which is more memory-efficient than the chunk-level DataEncryptor/DataDecryptor
// interfaces. The encrypt/decrypt services are available for non-streaming use.
type EncryptionCoordinator struct {
	tokenService     TokenIssuer
	keyGenService    KeyGenerator
	keyUnwrapService KeyUnwrapper
	keyStoreService  KeyStorer
	opaClient        *OPAClient
}

// CoordinatorConfig configuration for the coordinator
type CoordinatorConfig struct {
	TokenService     TokenIssuer
	KeyGenService    KeyGenerator
	KeyUnwrapService KeyUnwrapper
	KeyStoreService  KeyStorer
	OPAClient        *OPAClient
}

// NewEncryptionCoordinator creates a new encryption coordinator
func NewEncryptionCoordinator(cfg CoordinatorConfig) *EncryptionCoordinator {
	return &EncryptionCoordinator{
		tokenService:     cfg.TokenService,
		keyGenService:    cfg.KeyGenService,
		keyUnwrapService: cfg.KeyUnwrapService,
		keyStoreService:  cfg.KeyStoreService,
		opaClient:        cfg.OPAClient,
	}
}

// EncryptOperation performs a complete encryption operation using chunked,
// true streaming envelope encryption. It never buffers the entire object in
// memory. Returns ciphertext reader, keyID, metadata, ciphertext size, error.
func (c *EncryptionCoordinator) EncryptOperation(ctx context.Context, userID, bucket, objectKey string, plaintext io.Reader, objectSize int64) (io.Reader, string, []byte, int64, error) {
	// Step 1: Policy decision via OPA
	if c.opaClient != nil {
		allowed, err := c.opaClient.EvaluateAccess(ctx, UserContext{
			ID: userID,
		}, ObjectContext{
			Bucket: bucket,
			Key:    objectKey,
			Size:   objectSize,
		}, ActionContext{
			Type:      "write",
			Operation: "encrypt_object",
		}, RequestContext{
			Time: time.Now(),
		})
		if err != nil {
			zap.L().Error("opa policy evaluation failed", zap.Error(err))
			return nil, "", nil, 0, fmt.Errorf("policy evaluation failed: %w", err)
		}
		if !allowed {
			return nil, "", nil, 0, fmt.Errorf("access denied by policy")
		}
	}

	// Step 2: Generate client ECDH key pair for session
	clientECDHPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", nil, 0, fmt.Errorf("failed to generate client ECDH key: %w", err)
	}
	clientECDHPub := clientECDHPriv.PublicKey()

	// Step 3: Request write token from TokenService
	writeToken, err := c.tokenService.IssueWriteToken(ctx, userID, bucket, objectKey, 30)
	if err != nil {
		return nil, "", nil, 0, fmt.Errorf("failed to issue write token: %w", err)
	}

	// Step 4: Generate DEK from KeyGenService
	encryptedDEK, ecdhEncryptedDEK, serviceECDHPub, err := c.keyGenService.GenerateDataKey(
		ctx,
		writeToken.TokenID,
		userID,
		bucket,
		objectKey,
		&ECDHPublicKey{
			PublicKey: clientECDHPub.Bytes(),
			Curve:     "P-256",
		},
	)
	if err != nil {
		return nil, "", nil, 0, fmt.Errorf("failed to generate data key: %w", err)
	}

	// Step 5: Store encrypted DEK in KeyStoreService
	keyID, err := c.keyStoreService.StoreKey(bucket, objectKey, &EncryptedDEK{
		EncryptedKey: encryptedDEK.EncryptedKey,
		Algorithm:    encryptedDEK.Algorithm,
		KeyID:        encryptedDEK.KeyID,
		KeyVersion:   encryptedDEK.KeyVersion,
	}, objectSize)
	if err != nil {
		return nil, "", nil, 0, fmt.Errorf("failed to store key: %w", err)
	}

	// Step 6: Unwrap the DEK locally and stream-encrypt the plaintext.
	// The actual data encryption is done in chunked AES-256-GCM so the entire
	// object is never loaded into memory.
	sessionKey, err := deriveSessionKey(clientECDHPriv, serviceECDHPub)
	if err != nil {
		return nil, "", nil, 0, fmt.Errorf("failed to derive session key: %w", err)
	}
	defer clearBytes(sessionKey)

	dek, err := unwrapDEK(ecdhEncryptedDEK, sessionKey)
	if err != nil {
		return nil, "", nil, 0, fmt.Errorf("failed to unwrap DEK: %w", err)
	}
	defer clearBytes(dek)

	nonce := make([]byte, ssecNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", nil, 0, fmt.Errorf("failed to generate nonce: %w", err)
	}

	metadata := make([]byte, 0, 1+ssecNonceSize)
	metadata = append(metadata, envelopeMetadataVersion)
	metadata = append(metadata, nonce...)

	encryptReader := newStreamingEncryptReader(plaintext, dek, nonce)

	numChunks := int64(1)
	if objectSize > 0 {
		numChunks = (objectSize + ssecChunkSize - 1) / ssecChunkSize
	}
	estimatedCiphertextSize := objectSize + numChunks*(ssecCiphertextLenSize+ssecAuthTagSize)

	zap.L().Info("encryption completed",
		zap.String("key_id", keyID),
		zap.String("bucket", bucket),
		zap.String("object_key", objectKey),
		zap.Int64("plaintext_size", objectSize),
		zap.Int64("estimated_ciphertext_size", estimatedCiphertextSize))

	return encryptReader, keyID, metadata, estimatedCiphertextSize, nil
}

// DecryptOperation performs a complete decryption operation
func (c *EncryptionCoordinator) DecryptOperation(ctx context.Context, userID, bucket, objectKey string, ciphertext io.Reader, keyID string, metadata []byte) (io.Reader, error) {
	// Step 1: Policy decision via OPA
	if c.opaClient != nil {
		allowed, err := c.opaClient.EvaluateAccess(ctx, UserContext{
			ID: userID,
		}, ObjectContext{
			Bucket: bucket,
			Key:    objectKey,
		}, ActionContext{
			Type:      "read",
			Operation: "decrypt_object",
		}, RequestContext{
			Time: time.Now(),
		})
		if err != nil {
			zap.L().Error("opa policy evaluation failed", zap.Error(err))
			return nil, fmt.Errorf("policy evaluation failed: %w", err)
		}
		if !allowed {
			return nil, fmt.Errorf("access denied by policy")
		}
	}

	// Step 2: Generate client ECDH key pair for session
	clientECDHPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate client ECDH key: %w", err)
	}
	clientECDHPub := clientECDHPriv.PublicKey()

	// Step 3: Request read token from TokenService
	readToken, err := c.tokenService.IssueReadToken(ctx, userID, bucket, objectKey, "", 30)
	if err != nil {
		return nil, fmt.Errorf("failed to issue read token: %w", err)
	}

	// Step 4: Get encrypted DEK from KeyStoreService
	encryptedDEK, err := c.keyStoreService.GetKey(bucket, objectKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get key: %w", err)
	}

	// Step 5: Unwrap DEK from KeyUnwrapService
	ecdhEncryptedDEK, serviceECDHPub, err := c.keyUnwrapService.UnwrapKey(
		ctx,
		readToken.TokenID,
		userID,
		bucket,
		objectKey,
		encryptedDEK,
		&ECDHPublicKey{
			PublicKey: clientECDHPub.Bytes(),
			Curve:     "P-256",
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to unwrap key: %w", err)
	}

	// Step 6: Unwrap the DEK locally and stream-decrypt the ciphertext.
	sessionKey, err := deriveSessionKey(clientECDHPriv, serviceECDHPub)
	if err != nil {
		return nil, fmt.Errorf("failed to derive session key: %w", err)
	}
	defer clearBytes(sessionKey)

	dek, err := unwrapDEK(ecdhEncryptedDEK, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to unwrap DEK: %w", err)
	}
	defer clearBytes(dek)

	// Parse metadata: [version(1)][nonce(12)]
	if len(metadata) < 1+ssecNonceSize || metadata[0] != envelopeMetadataVersion {
		return nil, fmt.Errorf("invalid envelope metadata")
	}
	nonce := metadata[1 : 1+ssecNonceSize]

	decryptReader := newStreamingDecryptReader(ciphertext, dek, nonce)

	zap.L().Info("decryption started",
		zap.String("key_id", encryptedDEK.KeyID),
		zap.String("bucket", bucket),
		zap.String("object_key", objectKey))

	return decryptReader, nil
}

// DeleteKey deletes the key for an object
func (c *EncryptionCoordinator) DeleteKey(ctx context.Context, userID, bucket, objectKey string) error {
	// Step 1: Policy decision via OPA
	if c.opaClient != nil {
		allowed, err := c.opaClient.EvaluateAccess(ctx, UserContext{
			ID: userID,
		}, ObjectContext{
			Bucket: bucket,
			Key:    objectKey,
		}, ActionContext{
			Type:      "delete",
			Operation: "delete_object",
		}, RequestContext{
			Time: time.Now(),
		})
		if err != nil {
			return fmt.Errorf("policy evaluation failed: %w", err)
		}
		if !allowed {
			return fmt.Errorf("access denied by policy")
		}
	}

	// Step 2: Request delete token
	deleteToken, err := c.tokenService.IssueDeleteToken(ctx, userID, bucket, objectKey, 30)
	if err != nil {
		return fmt.Errorf("failed to issue delete token: %w", err)
	}

	// Step 3: Delete key from KeyStoreService
	if err := c.keyStoreService.DeleteKey(bucket, objectKey); err != nil {
		return fmt.Errorf("failed to delete key: %w", err)
	}

	zap.L().Info("key deleted",
		zap.String("token_id", deleteToken.TokenID),
		zap.String("bucket", bucket),
		zap.String("object_key", objectKey))

	return nil
}

// EncryptWithClientKey encrypts data using a customer-provided key (SSE-C).
// Uses streaming encryption to avoid loading the entire plaintext into memory.
func (c *EncryptionCoordinator) EncryptWithClientKey(ctx context.Context, plaintext io.Reader, clientKey []byte, objectSize int64) (io.Reader, []byte, int64, error) {
	if len(clientKey) != 32 {
		return nil, nil, 0, fmt.Errorf("invalid client key size: expected 32 bytes, got %d", len(clientKey))
	}

	nonce := make([]byte, ssecNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, 0, fmt.Errorf("failed to generate nonce: %w", err)
	}

	metadata := make([]byte, ssecNonceSize+ssecMetadataOriginalSizeLen)
	copy(metadata, nonce)
	binary.BigEndian.PutUint64(metadata[ssecNonceSize:], uint64(objectSize))

	encryptReader := newStreamingEncryptReader(plaintext, clientKey, nonce)

	numChunks := uint32(1)
	if objectSize > 0 {
		numChunks = uint32((objectSize + ssecChunkSize - 1) / ssecChunkSize)
	}
	estimatedCiphertextSize := objectSize + int64(numChunks)*(ssecCiphertextLenSize+ssecAuthTagSize)

	zap.L().Info("sse-c streaming encryption started",
		zap.Int64("object_size", objectSize))

	return encryptReader, metadata, estimatedCiphertextSize, nil
}

// DecryptWithClientKey decrypts data using a customer-provided key (SSE-C).
// Uses streaming decryption to avoid loading the entire ciphertext into memory.
func (c *EncryptionCoordinator) DecryptWithClientKey(ctx context.Context, ciphertext io.Reader, clientKey []byte, metadata []byte, objectSize int64) (io.Reader, error) {
	if len(clientKey) != 32 {
		return nil, fmt.Errorf("invalid client key size: expected 32 bytes, got %d", len(clientKey))
	}

	if len(metadata) < ssecNonceSize {
		return nil, fmt.Errorf("invalid metadata: too short")
	}
	nonce := metadata[:ssecNonceSize]

	decryptReader := newStreamingDecryptReader(ciphertext, clientKey, nonce)

	zap.L().Info("sse-c streaming decryption started")

	return decryptReader, nil
}

// Close closes all services
func (c *EncryptionCoordinator) Close() error {
	var errs []error

	if c.tokenService != nil {
		if err := c.tokenService.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.keyGenService != nil {
		if err := c.keyGenService.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.keyUnwrapService != nil {
		if err := c.keyUnwrapService.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.keyStoreService != nil {
		if err := c.keyStoreService.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing services: %v", errs)
	}

	return nil
}
