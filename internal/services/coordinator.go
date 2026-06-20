package services

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/hkdf"

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
// Main service uses this to orchestrate encryption/decryption operations
type EncryptionCoordinator struct {
	tokenService     TokenIssuer
	keyGenService    KeyGenerator
	keyUnwrapService KeyUnwrapper
	encryptService   DataEncryptor
	decryptService   DataDecryptor
	keyStoreService  KeyStorer
	opaClient        *OPAClient
}

// CoordinatorConfig configuration for the coordinator
type CoordinatorConfig struct {
	TokenService     TokenIssuer
	KeyGenService    KeyGenerator
	KeyUnwrapService KeyUnwrapper
	EncryptService   DataEncryptor
	DecryptService   DataDecryptor
	KeyStoreService  KeyStorer
	OPAClient        *OPAClient
}

// NewEncryptionCoordinator creates a new encryption coordinator
func NewEncryptionCoordinator(cfg CoordinatorConfig) *EncryptionCoordinator {
	return &EncryptionCoordinator{
		tokenService:     cfg.TokenService,
		keyGenService:    cfg.KeyGenService,
		keyUnwrapService: cfg.KeyUnwrapService,
		encryptService:   cfg.EncryptService,
		decryptService:   cfg.DecryptService,
		keyStoreService:  cfg.KeyStoreService,
		opaClient:        cfg.OPAClient,
	}
}

// deriveSessionKey derives a shared session key from an ECDH exchange.
// The info order matches KeyGenService: serverPub || clientPub.
func deriveSessionKey(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *ECDHPublicKey) ([]byte, error) {
	servicePub, err := ecdh.P256().NewPublicKey(serviceECDHPub.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse service ECDH public key: %w", err)
	}
	sharedSecret, err := clientECDHPriv.ECDH(servicePub)
	if err != nil {
		return nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}
	defer clearBytes(sharedSecret)

	info := append(servicePub.Bytes(), clientECDHPriv.PublicKey().Bytes()...)
	reader := hkdf.New(sha256.New, sharedSecret, nil, info)
	sessionKey := make([]byte, 32)
	if _, err := io.ReadFull(reader, sessionKey); err != nil {
		return nil, fmt.Errorf("failed to derive session key: %w", err)
	}
	return sessionKey, nil
}

// unwrapDEK decrypts the ECDH-encrypted DEK using the session key.
func unwrapDEK(ecdhEncryptedDEK *ECDHEncryptedDEK, sessionKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}
	dek, err := gcm.Open(nil, ecdhEncryptedDEK.Nonce, ecdhEncryptedDEK.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt DEK: %w", err)
	}
	return dek, nil
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
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

// Streaming SSE-C constants
const (
	// ssecChunkSize is the plaintext chunk size for streaming SSE-C encryption (32KB).
	ssecChunkSize = 32 * 1024
	// ssecNonceSize is the GCM nonce size (12 bytes).
	ssecNonceSize = 12
	// ssecAuthTagSize is the GCM authentication tag size (16 bytes).
	ssecAuthTagSize = 16
	// ssecCiphertextLenSize is the size of the ciphertext length prefix (4 bytes, uint32 big-endian).
	ssecCiphertextLenSize = 4
	// ssecLastChunkMarker is a special ciphertext length value marking the last chunk.
	// Regular chunks have length = actual ciphertext size; the last chunk has this marker OR-ed
	// into the MSB. Since ciphertext is at most ssecChunkSize + ssecAuthTagSize = 32784 bytes,
	// which fits in 15 bits, bit 31 is safe to use as a flag.
	ssecLastChunkMarker uint32 = 1 << 31
	// ssecMetadataLenSize is 8 bytes for storing original size (uint64 big-endian) after nonce.
	ssecMetadataOriginalSizeLen = 8
)

// ssecMetadataFormat: [nonce(12 bytes)][originalSize(8 bytes, uint64 big-endian)]
// Total metadata size = 12 + 8 = 20 bytes

// streamingEncryptReader implements io.Reader for streaming SSE-C encryption.
// It reads plaintext in chunks, encrypts each chunk with a derived key (HKDF from
// client key + chunk index), and outputs:
//
//	Output format: [nonce(12 bytes)][chunk1_encrypted][chunk2_encrypted]...
//	Each chunk: [ciphertext_len(4 bytes, uint32 big-endian, MSB set on last chunk)][ciphertext][authTag(16 bytes)]
type streamingEncryptReader struct {
	source   io.Reader
	key      []byte
	nonce    []byte // base nonce (12 bytes), stored in metadata
	chunkIdx uint32
	buf      []byte        // read buffer for plaintext
	outBuf   *bytes.Buffer // output buffer for encrypted chunk data
	done     bool
}

// newStreamingEncryptReader creates a new streaming encryptor.
func newStreamingEncryptReader(source io.Reader, key []byte, nonce []byte) *streamingEncryptReader {
	return &streamingEncryptReader{
		source: source,
		key:    key,
		nonce:  nonce,
		buf:    make([]byte, ssecChunkSize),
		outBuf: new(bytes.Buffer),
	}
}

// envelopeMetadataVersion is the first byte of envelope encryption metadata.
// Format: [version(1)][nonce(12)]
const envelopeMetadataVersion byte = 0x01

// deriveChunkKey derives a per-chunk encryption key using HKDF-SHA256.
// info = nonce || chunk_index (big-endian uint32)
func deriveChunkKey(key []byte, nonce []byte, chunkIdx uint32) []byte {
	info := make([]byte, len(nonce)+4)
	copy(info, nonce)
	binary.BigEndian.PutUint32(info[len(nonce):], chunkIdx)

	h := hmac.New(sha256.New, key)
	// HKDF-Extract: PRK = HMAC(key, nonce)
	h.Reset()
	h.Write(nonce)
	prk := h.Sum(nil)

	// HKDF-Expand: OKM = HMAC(PRK, info || 0x01)
	expander := hmac.New(sha256.New, prk)
	expander.Write(info)
	expander.Write([]byte{0x01})
	return expander.Sum(nil)
}

// encryptChunk encrypts a plaintext chunk with AES-256-GCM using a derived key.
// Returns: [ciphertext_len(4 bytes)][ciphertext][authTag(16 bytes)]
func encryptChunk(plaintext []byte, key []byte, nonce []byte, chunkIdx uint32, isLast bool) ([]byte, error) {
	derivedKey := deriveChunkKey(key, nonce, chunkIdx)

	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher for chunk %d: %w", chunkIdx, err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM for chunk %d: %w", chunkIdx, err)
	}

	// Use a chunk-specific nonce: base_nonce XOR chunk_index (first 4 bytes)
	chunkNonce := make([]byte, ssecNonceSize)
	copy(chunkNonce, nonce)
	// XOR chunk index into the first 4 bytes of the nonce
	for i := 0; i < 4; i++ {
		chunkNonce[i] ^= byte(chunkIdx >> (24 - 8*i))
	}

	sealed := gcm.Seal(nil, chunkNonce, plaintext, nil)

	// sealed = ciphertext || authTag
	if len(sealed) < ssecAuthTagSize {
		return nil, fmt.Errorf("sealed chunk too short")
	}
	ciphertext := sealed[:len(sealed)-ssecAuthTagSize]
	authTag := sealed[len(sealed)-ssecAuthTagSize:]

	// Build output: [ciphertext_len(4 bytes)][ciphertext][authTag]
	ctLen := uint32(len(ciphertext))
	if isLast {
		ctLen |= ssecLastChunkMarker
	}

	out := make([]byte, 0, ssecCiphertextLenSize+len(ciphertext)+ssecAuthTagSize)
	out = binary.BigEndian.AppendUint32(out, ctLen)
	out = append(out, ciphertext...)
	out = append(out, authTag...)

	return out, nil
}

func (r *streamingEncryptReader) Read(p []byte) (int, error) {
	for {
		// If we have buffered output, drain it first
		if r.outBuf.Len() > 0 {
			return r.outBuf.Read(p)
		}

		if r.done {
			return 0, io.EOF
		}

		// Read a chunk of plaintext
		n, readErr := io.ReadFull(r.source, r.buf)
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			return 0, readErr
		}

		isLast := readErr == io.EOF || readErr == io.ErrUnexpectedEOF || n == 0
		if n == 0 && isLast {
			// If we already wrote the nonce, and there's no data at all (empty plaintext),
			// we still need to write a last-chunk marker with zero-length ciphertext.
			if r.chunkIdx == 0 {
				chunkData, err := encryptChunk(nil, r.key, r.nonce, r.chunkIdx, true)
				if err != nil {
					return 0, err
				}
				r.outBuf.Write(chunkData)
				r.done = true
				continue
			}
			r.done = true
			continue
		}

		plaintext := r.buf[:n]

		// Encrypt the chunk.
		chunkData, err := encryptChunk(plaintext, r.key, r.nonce, r.chunkIdx, isLast)
		if err != nil {
			return 0, err
		}

		r.chunkIdx++
		r.outBuf.Write(chunkData)

		if isLast {
			r.done = true
		}
	}
}

// streamingDecryptReader implements io.Reader for streaming decryption.
type streamingDecryptReader struct {
	source  io.Reader
	key     []byte
	nonce   []byte
	chunkIdx uint32
	outBuf  *bytes.Buffer
	done    bool
	lenBuf  [ssecCiphertextLenSize]byte
	lenRead int
}

// newStreamingDecryptReader creates a new streaming decryptor.
func newStreamingDecryptReader(source io.Reader, key []byte, nonce []byte) *streamingDecryptReader {
	return &streamingDecryptReader{
		source: source,
		key:    key,
		nonce:  nonce,
		outBuf: new(bytes.Buffer),
	}
}

func (r *streamingDecryptReader) Read(p []byte) (int, error) {
	for {
		// Drain buffered plaintext first
		if r.outBuf.Len() > 0 {
			return r.outBuf.Read(p)
		}

		if r.done {
			return 0, io.EOF
		}

		// Read the 4-byte ciphertext length prefix
		for r.lenRead < ssecCiphertextLenSize {
			n, err := r.source.Read(r.lenBuf[r.lenRead:])
			if n > 0 {
				r.lenRead += n
			}
			if err != nil {
				if err == io.EOF && r.lenRead > 0 {
					return 0, fmt.Errorf("truncated chunk header")
				}
				return 0, err
			}
		}
		r.lenRead = 0

		ctLenField := binary.BigEndian.Uint32(r.lenBuf[:])
		isLast := (ctLenField & ssecLastChunkMarker) != 0
		ctLen := int(ctLenField & ^ssecLastChunkMarker)

		// Read ciphertext + authTag
		chunkData := make([]byte, ctLen+ssecAuthTagSize)
		if _, err := io.ReadFull(r.source, chunkData); err != nil {
			return 0, fmt.Errorf("failed to read chunk data: %w", err)
		}

		ciphertext := chunkData[:ctLen]
		authTag := chunkData[ctLen:]

		// Derive the key for this chunk
		derivedKey := deriveChunkKey(r.key, r.nonce, r.chunkIdx)

		block, err := aes.NewCipher(derivedKey)
		if err != nil {
			return 0, fmt.Errorf("decryption failed")
		}

		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return 0, fmt.Errorf("decryption failed")
		}

		// Reconstruct GCM sealed data: ciphertext || authTag
		gcmData := append(ciphertext, authTag...)

		// Compute chunk-specific nonce
		chunkNonce := make([]byte, ssecNonceSize)
		copy(chunkNonce, r.nonce)
		for i := 0; i < 4; i++ {
			chunkNonce[i] ^= byte(r.chunkIdx >> (24 - 8*i))
		}

		plaintext, err := gcm.Open(nil, chunkNonce, gcmData, nil)
		if err != nil {
			return 0, fmt.Errorf("decryption failed")
		}

		r.chunkIdx++
		r.outBuf.Write(plaintext)

		if isLast {
			r.done = true
		}
	}
}

// EncryptWithClientKey encrypts data using a customer-provided key (SSE-C).
// The client key is used directly as the DEK (no envelope encryption).
// Returns ciphertext reader, nonce+originalSize metadata, ciphertext size, error.
// The client key is NEVER persisted.
// Uses streaming encryption to avoid loading the entire plaintext into memory.
func (c *EncryptionCoordinator) EncryptWithClientKey(ctx context.Context, plaintext io.Reader, clientKey []byte, objectSize int64) (io.Reader, []byte, int64, error) {
	if len(clientKey) != 32 {
		return nil, nil, 0, fmt.Errorf("invalid client key size: expected 32 bytes, got %d", len(clientKey))
	}

	// Generate base nonce
	nonce := make([]byte, ssecNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, 0, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Build metadata: [nonce(12 bytes)][originalSize(8 bytes)]
	metadata := make([]byte, ssecNonceSize+ssecMetadataOriginalSizeLen)
	copy(metadata, nonce)
	binary.BigEndian.PutUint64(metadata[ssecNonceSize:], uint64(objectSize))

	// Create streaming encryptor
	// The ciphertext output does NOT include the nonce - it's stored in metadata.
	// The ciphertext stream is just: [chunk1][chunk2]...
	encryptReader := newStreamingEncryptReader(plaintext, clientKey, nonce)

	// Estimate ciphertext size: each chunk adds 4 (len prefix) + 16 (authTag) bytes overhead
	// For empty plaintext, we still have 1 chunk (with zero-length ciphertext)
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
// The client key is used directly as the DEK.
// Uses streaming decryption to avoid loading the entire ciphertext into memory.
func (c *EncryptionCoordinator) DecryptWithClientKey(ctx context.Context, ciphertext io.Reader, clientKey []byte, metadata []byte, objectSize int64) (io.Reader, error) {
	if len(clientKey) != 32 {
		return nil, fmt.Errorf("invalid client key size: expected 32 bytes, got %d", len(clientKey))
	}

	// Parse metadata: [nonce(12 bytes)][originalSize(8 bytes)]
	if len(metadata) < ssecNonceSize {
		return nil, fmt.Errorf("invalid metadata: too short")
	}
	nonce := metadata[:ssecNonceSize]

	// Create streaming decryptor
	decryptReader := newStreamingDecryptReader(ciphertext, clientKey, nonce)

	zap.L().Info("sse-c streaming decryption started")

	return decryptReader, nil
}

// ComputeSSECKeySHA256 computes the SHA-256 hash of a client key for storage verification.
func ComputeSSECKeySHA256(clientKey []byte) string {
	h := sha256.Sum256(clientKey)
	return fmt.Sprintf("%x", h[:])
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
	if c.encryptService != nil {
		if err := c.encryptService.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.decryptService != nil {
		if err := c.decryptService.Close(); err != nil {
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
