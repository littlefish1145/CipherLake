package kms

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalKMSRoundTrip(t *testing.T) {
	// Create a temp directory for keys
	tmpDir, err := os.MkdirTemp("", "kms-local-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "keygen")

	kmsClient, err := NewLocalKMS(LocalConfig{
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("failed to create LocalKMS: %v", err)
	}
	defer kmsClient.Close()

	ctx := context.Background()

	// Test GenerateDataKey with 32 bytes (AES-256)
	plaintext, encrypted, err := kmsClient.GenerateDataKey(ctx, "", 32)
	if err != nil {
		t.Fatalf("GenerateDataKey failed: %v", err)
	}
	if len(plaintext) != 32 {
		t.Errorf("expected plaintext length 32, got %d", len(plaintext))
	}
	if len(encrypted) == 0 {
		t.Error("expected non-empty encrypted key")
	}

	// Test DecryptDataKey round-trip
	decrypted, err := kmsClient.DecryptDataKey(ctx, "", encrypted)
	if err != nil {
		t.Fatalf("DecryptDataKey failed: %v", err)
	}
	if len(decrypted) != 32 {
		t.Errorf("expected decrypted length 32, got %d", len(decrypted))
	}
	if !equalBytes(plaintext, decrypted) {
		t.Errorf("decrypted key does not match original plaintext")
	}
}

func TestLocalKMSRoundTrip16(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "kms-local-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "keygen")

	kmsClient, err := NewLocalKMS(LocalConfig{
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("failed to create LocalKMS: %v", err)
	}
	defer kmsClient.Close()

	ctx := context.Background()

	// Test GenerateDataKey with 16 bytes (AES-128)
	plaintext, encrypted, err := kmsClient.GenerateDataKey(ctx, "", 16)
	if err != nil {
		t.Fatalf("GenerateDataKey failed: %v", err)
	}
	if len(plaintext) != 16 {
		t.Errorf("expected plaintext length 16, got %d", len(plaintext))
	}

	// Test DecryptDataKey round-trip
	decrypted, err := kmsClient.DecryptDataKey(ctx, "", encrypted)
	if err != nil {
		t.Fatalf("DecryptDataKey failed: %v", err)
	}
	if !equalBytes(plaintext, decrypted) {
		t.Errorf("decrypted key does not match original plaintext")
	}
}

func TestLocalKMSGetPublicKey(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "kms-local-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "keygen")

	kmsClient, err := NewLocalKMS(LocalConfig{
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("failed to create LocalKMS: %v", err)
	}
	defer kmsClient.Close()

	ctx := context.Background()

	// Test GetPublicKey
	pub, err := kmsClient.GetPublicKey(ctx, "")
	if err != nil {
		t.Fatalf("GetPublicKey failed: %v", err)
	}
	if len(pub) == 0 {
		t.Error("expected non-empty public key")
	}
}

func TestLocalKMSMultipleKeys(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "kms-local-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "keygen")

	kmsClient, err := NewLocalKMS(LocalConfig{
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("failed to create LocalKMS: %v", err)
	}
	defer kmsClient.Close()

	ctx := context.Background()

	// Generate multiple keys and verify each can be decrypted
	keys := make([][]byte, 5)
	encKeys := make([][]byte, 5)

	for i := 0; i < 5; i++ {
		plaintext, encrypted, err := kmsClient.GenerateDataKey(ctx, "", 32)
		if err != nil {
			t.Fatalf("GenerateDataKey %d failed: %v", i, err)
		}
		keys[i] = plaintext
		encKeys[i] = encrypted
	}

	// Verify each key can be decrypted correctly
	for i := 0; i < 5; i++ {
		decrypted, err := kmsClient.DecryptDataKey(ctx, "", encKeys[i])
		if err != nil {
			t.Fatalf("DecryptDataKey %d failed: %v", i, err)
		}
		if !equalBytes(keys[i], decrypted) {
			t.Errorf("key %d: decrypted does not match original", i)
		}
	}
}

func TestLocalKMSReloadKeys(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "kms-local-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "keygen")

	// Create first KMS instance and generate a key
	kms1, err := NewLocalKMS(LocalConfig{
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("failed to create first LocalKMS: %v", err)
	}

	ctx := context.Background()
	plaintext, encrypted, err := kms1.GenerateDataKey(ctx, "", 32)
	if err != nil {
		t.Fatalf("GenerateDataKey failed: %v", err)
	}
	kms1.Close()

	// Create second KMS instance loading the same keys
	kms2, err := NewLocalKMS(LocalConfig{
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("failed to create second LocalKMS: %v", err)
	}
	defer kms2.Close()

	// Verify the key can still be decrypted with the reloaded keys
	decrypted, err := kms2.DecryptDataKey(ctx, "", encrypted)
	if err != nil {
		t.Fatalf("DecryptDataKey with reloaded keys failed: %v", err)
	}
	if !equalBytes(plaintext, decrypted) {
		t.Errorf("decrypted key does not match original after reload")
	}
}

func TestLocalKMSInvalidEncryptedData(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "kms-local-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "keygen")

	kmsClient, err := NewLocalKMS(LocalConfig{
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("failed to create LocalKMS: %v", err)
	}
	defer kmsClient.Close()

	ctx := context.Background()

	// Try to decrypt garbage data
	_, err = kmsClient.DecryptDataKey(ctx, "", []byte("garbage-data"))
	if err == nil {
		t.Error("expected error when decrypting invalid data")
	}

	// Try to decrypt empty data
	_, err = kmsClient.DecryptDataKey(ctx, "", []byte{})
	if err == nil {
		t.Error("expected error when decrypting empty data")
	}

	// Try to decrypt too-short data
	_, err = kmsClient.DecryptDataKey(ctx, "", []byte{1, 2, 3})
	if err == nil {
		t.Error("expected error when decrypting too-short data")
	}
}

func TestLocalKMSDefaultLength(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "kms-local-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "keygen")

	kmsClient, err := NewLocalKMS(LocalConfig{
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("failed to create LocalKMS: %v", err)
	}
	defer kmsClient.Close()

	ctx := context.Background()

	// Test with length 0 (should default to 32)
	plaintext, encrypted, err := kmsClient.GenerateDataKey(ctx, "", 0)
	if err != nil {
		t.Fatalf("GenerateDataKey with length 0 failed: %v", err)
	}
	if len(plaintext) != 32 {
		t.Errorf("expected default plaintext length 32, got %d", len(plaintext))
	}

	// Verify round-trip
	decrypted, err := kmsClient.DecryptDataKey(ctx, "", encrypted)
	if err != nil {
		t.Fatalf("DecryptDataKey failed: %v", err)
	}
	if !equalBytes(plaintext, decrypted) {
		t.Errorf("decrypted key does not match original")
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
