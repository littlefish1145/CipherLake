package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCoordinator() *EncryptionCoordinator {
	return NewEncryptionCoordinator(CoordinatorConfig{})
}

func TestEncryptWithClientKey_DecryptWithClientKey_RoundTrip(t *testing.T) {
	coordinator := newTestCoordinator()
	ctx := context.Background()

	clientKey := make([]byte, 32)
	_, err := rand.Read(clientKey)
	require.NoError(t, err)

	plaintext := []byte("Hello, SSE-C encryption world! This is a test of AES-256-GCM with customer-provided keys.")
	plaintextReader := bytes.NewReader(plaintext)

	encryptedReader, metadata, ciphertextSize, err := coordinator.EncryptWithClientKey(ctx, plaintextReader, clientKey, int64(len(plaintext)))
	require.NoError(t, err)
	require.NotNil(t, encryptedReader)
	require.NotNil(t, metadata)
	assert.Greater(t, ciphertextSize, int64(0))

	// Read the ciphertext
	ciphertext, err := io.ReadAll(encryptedReader)
	require.NoError(t, err)

	// Decrypt
	ciphertextReader := bytes.NewReader(ciphertext)
	decryptedReader, err := coordinator.DecryptWithClientKey(ctx, ciphertextReader, clientKey, metadata, ciphertextSize)
	require.NoError(t, err)
	require.NotNil(t, decryptedReader)

	decrypted, err := io.ReadAll(decryptedReader)
	require.NoError(t, err)

	assert.Equal(t, plaintext, decrypted)
}

func TestEncryptWithClientKey_InvalidKeySize(t *testing.T) {
	coordinator := newTestCoordinator()
	ctx := context.Background()

	// Key too short
	shortKey := make([]byte, 16)
	_, err := rand.Read(shortKey)
	require.NoError(t, err)

	plaintext := []byte("test data")
	plaintextReader := bytes.NewReader(plaintext)

	_, _, _, err = coordinator.EncryptWithClientKey(ctx, plaintextReader, shortKey, int64(len(plaintext)))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestEncryptWithClientKey_DecryptWithWrongKey(t *testing.T) {
	coordinator := newTestCoordinator()
	ctx := context.Background()

	clientKey := make([]byte, 32)
	_, err := rand.Read(clientKey)
	require.NoError(t, err)

	wrongKey := make([]byte, 32)
	_, err = rand.Read(wrongKey)
	require.NoError(t, err)

	plaintext := []byte("Secret data encrypted with SSE-C")
	plaintextReader := bytes.NewReader(plaintext)

	encryptedReader, metadata, ciphertextSize, err := coordinator.EncryptWithClientKey(ctx, plaintextReader, clientKey, int64(len(plaintext)))
	require.NoError(t, err)

	ciphertext, err := io.ReadAll(encryptedReader)
	require.NoError(t, err)

	// Try to decrypt with wrong key
	ciphertextReader := bytes.NewReader(ciphertext)
	decryptedReader, err := coordinator.DecryptWithClientKey(ctx, ciphertextReader, wrongKey, metadata, ciphertextSize)
	require.NoError(t, err) // DecryptWithClientKey returns a streaming reader, error occurs on read
	_, err = io.ReadAll(decryptedReader)
	assert.Error(t, err)
}

func TestEncryptWithClientKey_LargeData(t *testing.T) {
	coordinator := newTestCoordinator()
	ctx := context.Background()

	clientKey := make([]byte, 32)
	_, err := rand.Read(clientKey)
	require.NoError(t, err)

	// 1MB of data
	plaintext := make([]byte, 1024*1024)
	_, err = rand.Read(plaintext)
	require.NoError(t, err)

	plaintextReader := bytes.NewReader(plaintext)

	encryptedReader, metadata, ciphertextSize, err := coordinator.EncryptWithClientKey(ctx, plaintextReader, clientKey, int64(len(plaintext)))
	require.NoError(t, err)

	ciphertext, err := io.ReadAll(encryptedReader)
	require.NoError(t, err)

	ciphertextReader := bytes.NewReader(ciphertext)
	decryptedReader, err := coordinator.DecryptWithClientKey(ctx, ciphertextReader, clientKey, metadata, ciphertextSize)
	require.NoError(t, err)

	decrypted, err := io.ReadAll(decryptedReader)
	require.NoError(t, err)

	assert.Equal(t, plaintext, decrypted)
}

func TestEncryptWithClientKey_MetadataFormat(t *testing.T) {
	coordinator := newTestCoordinator()
	ctx := context.Background()

	clientKey := make([]byte, 32)
	_, err := rand.Read(clientKey)
	require.NoError(t, err)

	plaintext := []byte("test")
	plaintextReader := bytes.NewReader(plaintext)

	_, metadata, _, err := coordinator.EncryptWithClientKey(ctx, plaintextReader, clientKey, int64(len(plaintext)))
	require.NoError(t, err)

	// Metadata should be nonce (12 bytes) + originalSize (8 bytes) = 20 bytes
	assert.Equal(t, 20, len(metadata))
}

func TestDecryptWithClientKey_InvalidMetadata(t *testing.T) {
	coordinator := newTestCoordinator()
	ctx := context.Background()

	clientKey := make([]byte, 32)
	_, err := rand.Read(clientKey)
	require.NoError(t, err)

	// Metadata too short
	shortMetadata := make([]byte, 10)
	ciphertext := []byte("some ciphertext data")

	ciphertextReader := bytes.NewReader(ciphertext)
	_, err = coordinator.DecryptWithClientKey(ctx, ciphertextReader, clientKey, shortMetadata, int64(len(ciphertext)))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestDecryptWithClientKey_InvalidKeySize(t *testing.T) {
	coordinator := newTestCoordinator()
	ctx := context.Background()

	shortKey := make([]byte, 16)
	_, err := rand.Read(shortKey)
	require.NoError(t, err)

	metadata := make([]byte, 20)
	ciphertext := []byte("some ciphertext data")

	ciphertextReader := bytes.NewReader(ciphertext)
	_, err = coordinator.DecryptWithClientKey(ctx, ciphertextReader, shortKey, metadata, int64(len(ciphertext)))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestComputeSSECKeySHA256(t *testing.T) {
	clientKey := make([]byte, 32)
	// Use a fixed key for deterministic test
	for i := range clientKey {
		clientKey[i] = byte(i)
	}

	sha256Hash := ComputeSSECKeySHA256(clientKey)
	assert.NotEmpty(t, sha256Hash)
	assert.Len(t, sha256Hash, 64) // SHA-256 hex string is 64 characters

	// Same key should produce same hash
	sha256Hash2 := ComputeSSECKeySHA256(clientKey)
	assert.Equal(t, sha256Hash, sha256Hash2)

	// Different key should produce different hash
	differentKey := make([]byte, 32)
	for i := range differentKey {
		differentKey[i] = byte(i + 1)
	}
	sha256Hash3 := ComputeSSECKeySHA256(differentKey)
	assert.NotEqual(t, sha256Hash, sha256Hash3)
}

func TestComputeSSECKeySHA256_KeyVerification(t *testing.T) {
	// Simulate the full flow: encrypt, store SHA-256, then verify on decrypt
	clientKey := make([]byte, 32)
	_, err := rand.Read(clientKey)
	require.NoError(t, err)

	// Compute SHA-256 at encryption time (stored in metadata)
	storedSHA256 := ComputeSSECKeySHA256(clientKey)

	// At decryption time, recompute SHA-256 and verify
	providedKey := clientKey
	providedSHA256 := ComputeSSECKeySHA256(providedKey)
	assert.Equal(t, storedSHA256, providedSHA256)

	// Wrong key should not match
	wrongKey := make([]byte, 32)
	_, err = rand.Read(wrongKey)
	require.NoError(t, err)
	wrongSHA256 := ComputeSSECKeySHA256(wrongKey)
	assert.NotEqual(t, storedSHA256, wrongSHA256)
}

func TestEncryptWithClientKey_EmptyPlaintext(t *testing.T) {
	coordinator := newTestCoordinator()
	ctx := context.Background()

	clientKey := make([]byte, 32)
	_, err := rand.Read(clientKey)
	require.NoError(t, err)

	plaintext := []byte("")
	plaintextReader := bytes.NewReader(plaintext)

	encryptedReader, metadata, ciphertextSize, err := coordinator.EncryptWithClientKey(ctx, plaintextReader, clientKey, int64(len(plaintext)))
	require.NoError(t, err)

	ciphertext, err := io.ReadAll(encryptedReader)
	require.NoError(t, err)

	ciphertextReader := bytes.NewReader(ciphertext)
	decryptedReader, err := coordinator.DecryptWithClientKey(ctx, ciphertextReader, clientKey, metadata, ciphertextSize)
	require.NoError(t, err)

	decrypted, err := io.ReadAll(decryptedReader)
	require.NoError(t, err)

	assert.Equal(t, plaintext, decrypted)
}
