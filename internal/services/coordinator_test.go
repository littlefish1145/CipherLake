package services

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/hkdf"

	"nexus/internal/services/token_service"
)

// mockTokenIssuer implements TokenIssuer for testing.
type mockTokenIssuer struct {
	mu     sync.Mutex
	tokens map[string]*token_service.DelegationToken
}

func newMockTokenIssuer() *mockTokenIssuer {
	return &mockTokenIssuer{tokens: make(map[string]*token_service.DelegationToken)}
}

func (m *mockTokenIssuer) issueToken() *token_service.DelegationToken {
	tok := &token_service.DelegationToken{
		TokenID: fmt.Sprintf("tok-%d", len(m.tokens)),
	}
	m.mu.Lock()
	m.tokens[tok.TokenID] = tok
	m.mu.Unlock()
	return tok
}

func (m *mockTokenIssuer) IssueWriteToken(ctx context.Context, userID, bucket, objectKey string, ttlSeconds int64) (*token_service.DelegationToken, error) {
	return m.issueToken(), nil
}

func (m *mockTokenIssuer) IssueReadToken(ctx context.Context, userID, bucket, objectKey, contentHash string, ttlSeconds int64) (*token_service.DelegationToken, error) {
	return m.issueToken(), nil
}

func (m *mockTokenIssuer) IssueDeleteToken(ctx context.Context, userID, bucket, objectKey string, ttlSeconds int64) (*token_service.DelegationToken, error) {
	return m.issueToken(), nil
}

func (m *mockTokenIssuer) Close() error { return nil }

// mockKeyGenUnwrap pairs KeyGenerator and KeyUnwrapper with a fixed DEK.
type mockKeyGenUnwrap struct {
	mu          sync.Mutex
	keys        map[string]*EncryptedDEK
	deks        map[string][]byte
	servicePriv *ecdh.PrivateKey
	servicePub  *ecdh.PublicKey
}

func newMockKeyGenUnwrap() (*mockKeyGenUnwrap, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &mockKeyGenUnwrap{
		keys:        make(map[string]*EncryptedDEK),
		deks:        make(map[string][]byte),
		servicePriv: priv,
		servicePub:  priv.PublicKey(),
	}, nil
}

func (m *mockKeyGenUnwrap) GenerateDataKey(ctx context.Context, tokenID, userID, bucket, objectKey string, clientECDHPub *ECDHPublicKey) (*EncryptedDEK, *ECDHEncryptedDEK, *ECDHPublicKey, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, nil, err
	}

	clientPub, err := ecdh.P256().NewPublicKey(clientECDHPub.PublicKey)
	if err != nil {
		return nil, nil, nil, err
	}
	sharedSecret, err := m.servicePriv.ECDH(clientPub)
	if err != nil {
		return nil, nil, nil, err
	}
	defer clearBytes(sharedSecret)

	info := append(m.servicePub.Bytes(), clientPub.Bytes()...)
	sessionKey := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, sharedSecret, nil, info), sessionKey); err != nil {
		return nil, nil, nil, err
	}
	defer clearBytes(sessionKey)

	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, nil, err
	}
	encryptedDEK := gcm.Seal(nil, nonce, dek, nil)

	clearBytes(dek)

	encDEK := &EncryptedDEK{
		EncryptedKey: encryptedDEK,
		Algorithm:    "AES-256-GCM",
		KeyID:        "test-key-id",
		KeyVersion:   1,
	}
	ecdhEncDEK := &ECDHEncryptedDEK{
		Ciphertext: encryptedDEK,
		Nonce:      nonce,
	}

	m.mu.Lock()
	m.keys[bucket+"/"+objectKey] = encDEK
	m.deks[bucket+"/"+objectKey] = append([]byte(nil), dek...)
	m.mu.Unlock()

	clearBytes(dek)

	return encDEK, ecdhEncDEK, &ECDHPublicKey{PublicKey: m.servicePub.Bytes(), Curve: "P-256"}, nil
}

func (m *mockKeyGenUnwrap) GetPublicKey() ([]byte, string, string) {
	return m.servicePub.Bytes(), "P-256", "test-key-id"
}

func (m *mockKeyGenUnwrap) Close() error { return nil }

func (m *mockKeyGenUnwrap) UnwrapKey(ctx context.Context, tokenID, userID, bucket, objectKey string, encryptedDEK *EncryptedDEK, clientECDHPub *ECDHPublicKey) (*ECDHEncryptedDEK, *ECDHPublicKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dek, ok := m.deks[bucket+"/"+objectKey]
	if !ok {
		return nil, nil, fmt.Errorf("dek not found")
	}

	clientPub, err := ecdh.P256().NewPublicKey(clientECDHPub.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	sharedSecret, err := m.servicePriv.ECDH(clientPub)
	if err != nil {
		return nil, nil, err
	}
	defer clearBytes(sharedSecret)

	info := append(m.servicePub.Bytes(), clientPub.Bytes()...)
	sessionKey := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, sharedSecret, nil, info), sessionKey); err != nil {
		return nil, nil, err
	}
	defer clearBytes(sessionKey)

	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	encryptedDEKBytes := gcm.Seal(nil, nonce, dek, nil)

	return &ECDHEncryptedDEK{
		Ciphertext: encryptedDEKBytes,
		Nonce:      nonce,
	}, &ECDHPublicKey{PublicKey: m.servicePub.Bytes(), Curve: "P-256"}, nil
}

// mockKeyStorer implements KeyStorer in memory.
type mockKeyStorer struct {
	mu   sync.Mutex
	keys map[string]*EncryptedDEK
}

func newMockKeyStorer() *mockKeyStorer {
	return &mockKeyStorer{keys: make(map[string]*EncryptedDEK)}
}

func (m *mockKeyStorer) StoreKey(bucket, objectKey string, encryptedDEK *EncryptedDEK, objectSize int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[bucket+"/"+objectKey] = encryptedDEK
	return encryptedDEK.KeyID, nil
}

func (m *mockKeyStorer) GetKey(bucket, objectKey string) (*EncryptedDEK, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[bucket+"/"+objectKey]
	if !ok {
		return nil, fmt.Errorf("key not found")
	}
	return k, nil
}

func (m *mockKeyStorer) DeleteKey(bucket, objectKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, bucket+"/"+objectKey)
	return nil
}

func (m *mockKeyStorer) Close() error { return nil }

func newTestEnvelopeCoordinator(t *testing.T) *EncryptionCoordinator {
	t.Helper()
	kg, err := newMockKeyGenUnwrap()
	require.NoError(t, err)

	return NewEncryptionCoordinator(CoordinatorConfig{
		TokenService:     newMockTokenIssuer(),
		KeyGenService:    kg,
		KeyUnwrapService: kg,
		EncryptService:   nil,
		DecryptService:   nil,
		KeyStoreService:  newMockKeyStorer(),
	})
}

func TestEncryptOperation_DecryptOperation_RoundTrip(t *testing.T) {
	coordinator := newTestEnvelopeCoordinator(t)
	ctx := context.Background()

	plaintext := []byte("Hello, streaming envelope encryption!")
	encryptedReader, keyID, metadata, ciphertextSize, err := coordinator.EncryptOperation(
		ctx, "user-1", "bucket-1", "object-1", bytes.NewReader(plaintext), int64(len(plaintext)),
	)
	require.NoError(t, err)
	require.NotEmpty(t, keyID)
	require.Greater(t, ciphertextSize, int64(0))

	ciphertext, err := io.ReadAll(encryptedReader)
	require.NoError(t, err)

	decryptedReader, err := coordinator.DecryptOperation(
		ctx, "user-1", "bucket-1", "object-1", bytes.NewReader(ciphertext), keyID, metadata,
	)
	require.NoError(t, err)

	decrypted, err := io.ReadAll(decryptedReader)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestEncryptOperation_DecryptOperation_LargeData(t *testing.T) {
	coordinator := newTestEnvelopeCoordinator(t)
	ctx := context.Background()

	// 2MB of data to exercise multiple chunks.
	plaintext := make([]byte, 2*1024*1024)
	_, err := rand.Read(plaintext)
	require.NoError(t, err)

	encryptedReader, keyID, metadata, _, err := coordinator.EncryptOperation(
		ctx, "user-1", "bucket-2", "object-2", bytes.NewReader(plaintext), int64(len(plaintext)),
	)
	require.NoError(t, err)

	ciphertext, err := io.ReadAll(encryptedReader)
	require.NoError(t, err)

	decryptedReader, err := coordinator.DecryptOperation(
		ctx, "user-1", "bucket-2", "object-2", bytes.NewReader(ciphertext), keyID, metadata,
	)
	require.NoError(t, err)

	decrypted, err := io.ReadAll(decryptedReader)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestEncryptOperation_DecryptOperation_EmptyPlaintext(t *testing.T) {
	coordinator := newTestEnvelopeCoordinator(t)
	ctx := context.Background()

	plaintext := []byte("")
	encryptedReader, keyID, metadata, _, err := coordinator.EncryptOperation(
		ctx, "user-1", "bucket-3", "object-3", bytes.NewReader(plaintext), 0,
	)
	require.NoError(t, err)

	ciphertext, err := io.ReadAll(encryptedReader)
	require.NoError(t, err)

	decryptedReader, err := coordinator.DecryptOperation(
		ctx, "user-1", "bucket-3", "object-3", bytes.NewReader(ciphertext), keyID, metadata,
	)
	require.NoError(t, err)

	decrypted, err := io.ReadAll(decryptedReader)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestDecryptOperation_InvalidMetadata(t *testing.T) {
	coordinator := newTestEnvelopeCoordinator(t)
	ctx := context.Background()

	// Create a key entry first so GetKey succeeds and metadata parsing is reached.
	_, _, _, _, err := coordinator.EncryptOperation(
		ctx, "user-1", "bucket-4", "object-4", bytes.NewReader([]byte("x")), 1,
	)
	require.NoError(t, err)

	_, err = coordinator.DecryptOperation(
		ctx, "user-1", "bucket-4", "object-4", bytes.NewReader([]byte("x")), "", []byte{0xFF},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid envelope metadata")
}
