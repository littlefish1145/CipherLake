package kms

import (
	"context"
	"fmt"
	"testing"
)

// mockKMS is a simple mock implementation of KMSClient for testing.
type mockKMS struct {
	generateErr error
	decryptErr  error
	pubKeyErr   error
	plaintext   []byte
	encrypted   []byte
	pubKey      []byte
}

func (m *mockKMS) GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error) {
	if m.generateErr != nil {
		return nil, nil, m.generateErr
	}
	return m.plaintext, m.encrypted, nil
}

func (m *mockKMS) DecryptDataKey(ctx context.Context, keyID string, encrypted []byte) (plaintext []byte, err error) {
	if m.decryptErr != nil {
		return nil, m.decryptErr
	}
	return m.plaintext, nil
}

func (m *mockKMS) GetPublicKey(ctx context.Context, keyID string) (pub []byte, err error) {
	if m.pubKeyErr != nil {
		return nil, m.pubKeyErr
	}
	return m.pubKey, nil
}

func (m *mockKMS) Close() error {
	return nil
}

// TestKMSClientInterface verifies that the KMSClient interface can be implemented.
func TestKMSClientInterface(t *testing.T) {
	var _ KMSClient = (*mockKMS)(nil)
	var _ KMSClient = (*LocalKMS)(nil)
	var _ KMSClient = (*VaultTransitKMS)(nil)
	var _ KMSClient = (*AWSKMS)(nil)
	var _ KMSClient = (*FallbackKMS)(nil)
}

// TestMockKMSBasic tests the mock KMS for basic functionality.
func TestMockKMSBasic(t *testing.T) {
	m := &mockKMS{
		plaintext: []byte("test-key-data-1234567890123456"),
		encrypted: []byte("encrypted-key-blob"),
		pubKey:    []byte("public-key-data"),
	}

	ctx := context.Background()

	// Test GenerateDataKey
	plaintext, encrypted, err := m.GenerateDataKey(ctx, "test-key", 32)
	if err != nil {
		t.Fatalf("GenerateDataKey failed: %v", err)
	}
	if string(plaintext) != "test-key-data-1234567890123456" {
		t.Errorf("unexpected plaintext: %s", plaintext)
	}
	if string(encrypted) != "encrypted-key-blob" {
		t.Errorf("unexpected encrypted: %s", encrypted)
	}

	// Test DecryptDataKey
	decrypted, err := m.DecryptDataKey(ctx, "test-key", encrypted)
	if err != nil {
		t.Fatalf("DecryptDataKey failed: %v", err)
	}
	if string(decrypted) != "test-key-data-1234567890123456" {
		t.Errorf("unexpected decrypted: %s", decrypted)
	}

	// Test GetPublicKey
	pub, err := m.GetPublicKey(ctx, "test-key")
	if err != nil {
		t.Fatalf("GetPublicKey failed: %v", err)
	}
	if string(pub) != "public-key-data" {
		t.Errorf("unexpected public key: %s", pub)
	}

	// Test Close
	if err := m.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// TestMockKMSErrors tests the mock KMS error handling.
func TestMockKMSErrors(t *testing.T) {
	m := &mockKMS{
		generateErr: fmt.Errorf("generate error"),
		decryptErr:  fmt.Errorf("decrypt error"),
		pubKeyErr:   fmt.Errorf("pubkey error"),
	}

	ctx := context.Background()

	_, _, err := m.GenerateDataKey(ctx, "test-key", 32)
	if err == nil || err.Error() != "generate error" {
		t.Errorf("expected generate error, got: %v", err)
	}

	_, err = m.DecryptDataKey(ctx, "test-key", nil)
	if err == nil || err.Error() != "decrypt error" {
		t.Errorf("expected decrypt error, got: %v", err)
	}

	_, err = m.GetPublicKey(ctx, "test-key")
	if err == nil || err.Error() != "pubkey error" {
		t.Errorf("expected pubkey error, got: %v", err)
	}
}
