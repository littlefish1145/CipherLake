package kms

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// failingKMS is a KMSClient that always returns errors.
type failingKMS struct {
	err error
}

func newFailingKMS(err error) *failingKMS {
	return &failingKMS{err: err}
}

func (f *failingKMS) GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error) {
	return nil, nil, f.err
}

func (f *failingKMS) DecryptDataKey(ctx context.Context, keyID string, encrypted []byte) (plaintext []byte, err error) {
	return nil, f.err
}

func (f *failingKMS) GetPublicKey(ctx context.Context, keyID string) (pub []byte, err error) {
	return nil, f.err
}

func (f *failingKMS) Close() error {
	return nil
}

// eventuallyRecoveringKMS fails for the first N calls then succeeds.
type eventuallyRecoveringKMS struct {
	failCount    int
	currentCount int
	plaintext    []byte
	encrypted    []byte
	pubKey       []byte
}

func (e *eventuallyRecoveringKMS) GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error) {
	e.currentCount++
	if e.currentCount <= e.failCount {
		return nil, nil, fmt.Errorf("KMS not available yet (attempt %d)", e.currentCount)
	}
	return e.plaintext, e.encrypted, nil
}

func (e *eventuallyRecoveringKMS) DecryptDataKey(ctx context.Context, keyID string, encrypted []byte) (plaintext []byte, err error) {
	e.currentCount++
	if e.currentCount <= e.failCount {
		return nil, fmt.Errorf("KMS not available yet (attempt %d)", e.currentCount)
	}
	return e.plaintext, nil
}

func (e *eventuallyRecoveringKMS) GetPublicKey(ctx context.Context, keyID string) (pub []byte, err error) {
	e.currentCount++
	if e.currentCount <= e.failCount {
		return nil, fmt.Errorf("KMS not available yet (attempt %d)", e.currentCount)
	}
	return e.pubKey, nil
}

func (e *eventuallyRecoveringKMS) Close() error {
	return nil
}

func TestFallbackKMSRejectWrites(t *testing.T) {
	mock := &mockKMS{
		plaintext: []byte("test-dek-32-bytes-long-key!!"),
		encrypted: []byte("encrypted-dek-blob"),
		pubKey:    []byte("public-key"),
	}

	fk, err := NewFallbackKMS(FallbackConfig{
		Primary:             mock,
		Mode:                RejectWrites,
		HealthCheckInterval: 1 * time.Hour, // disable health checks for this test
	})
	if err != nil {
		t.Fatalf("failed to create FallbackKMS: %v", err)
	}
	defer fk.Close()

	ctx := context.Background()

	// Initially available - should succeed
	if !fk.IsAvailable() {
		t.Error("expected KMS to be available initially")
	}

	plaintext, encrypted, err := fk.GenerateDataKey(ctx, "", 32)
	if err != nil {
		t.Fatalf("GenerateDataKey failed: %v", err)
	}
	if string(plaintext) != "test-dek-32-bytes-long-key!!" {
		t.Errorf("unexpected plaintext: %s", plaintext)
	}

	// Decrypt should work
	decrypted, err := fk.DecryptDataKey(ctx, "", encrypted)
	if err != nil {
		t.Fatalf("DecryptDataKey failed: %v", err)
	}
	if string(decrypted) != "test-dek-32-bytes-long-key!!" {
		t.Errorf("unexpected decrypted: %s", decrypted)
	}
}

func TestFallbackKMSDegradedMode(t *testing.T) {
	// Start with a working KMS, then switch to a failing one
	mock := &mockKMS{
		plaintext: []byte("test-dek-32-bytes-long-key!!"),
		encrypted: []byte("encrypted-dek-blob"),
		pubKey:    []byte("public-key"),
	}

	fk, err := NewFallbackKMS(FallbackConfig{
		Primary:             mock,
		Mode:                RejectWrites,
		HealthCheckInterval: 1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("failed to create FallbackKMS: %v", err)
	}
	defer fk.Close()

	ctx := context.Background()

	// Generate a key while KMS is available
	plaintext, encrypted, err := fk.GenerateDataKey(ctx, "", 32)
	if err != nil {
		t.Fatalf("GenerateDataKey failed: %v", err)
	}

	// Now simulate KMS failure by replacing the primary with a failing one
	// We need to manually mark it as unavailable
	fk.markUnavailable(fmt.Errorf("simulated failure"))

	if fk.IsAvailable() {
		t.Error("expected KMS to be unavailable after marking")
	}

	// GenerateDataKey should fail when KMS is unavailable
	_, _, err = fk.GenerateDataKey(ctx, "", 32)
	if err == nil {
		t.Error("expected GenerateDataKey to fail when KMS unavailable")
	}

	// DecryptDataKey should still work using cached keys
	decrypted, err := fk.DecryptDataKey(ctx, "", encrypted)
	if err != nil {
		t.Fatalf("DecryptDataKey with cache should succeed: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Errorf("decrypted key does not match original")
	}

	// DecryptDataKey with unknown key should fail
	_, err = fk.DecryptDataKey(ctx, "", []byte("unknown-encrypted-key"))
	if err == nil {
		t.Error("expected DecryptDataKey to fail for unknown key when KMS unavailable")
	}
}

func TestFallbackKMSReadOnlyMode(t *testing.T) {
	mock := &mockKMS{
		plaintext: []byte("test-dek-32-bytes-long-key!!"),
		encrypted: []byte("encrypted-dek-blob"),
		pubKey:    []byte("public-key"),
	}

	fk, err := NewFallbackKMS(FallbackConfig{
		Primary:             mock,
		Mode:                ReadOnly,
		HealthCheckInterval: 1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("failed to create FallbackKMS: %v", err)
	}
	defer fk.Close()

	ctx := context.Background()

	// Generate a key while KMS is available
	plaintext, encrypted, err := fk.GenerateDataKey(ctx, "", 32)
	if err != nil {
		t.Fatalf("GenerateDataKey failed: %v", err)
	}

	// Mark as unavailable
	fk.markUnavailable(fmt.Errorf("simulated failure"))

	// GenerateDataKey should fail in read_only mode too
	_, _, err = fk.GenerateDataKey(ctx, "", 32)
	if err == nil {
		t.Error("expected GenerateDataKey to fail in read_only mode when KMS unavailable")
	}

	// DecryptDataKey should work using cached keys
	decrypted, err := fk.DecryptDataKey(ctx, "", encrypted)
	if err != nil {
		t.Fatalf("DecryptDataKey with cache should succeed: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Errorf("decrypted key does not match original")
	}
}

func TestFallbackKMSRecovery(t *testing.T) {
	recovering := &eventuallyRecoveringKMS{
		failCount: 2,
		plaintext: []byte("recovered-dek-32-bytes-long!!!"),
		encrypted: []byte("recovered-encrypted"),
		pubKey:    []byte("recovered-pubkey"),
	}

	fk, err := NewFallbackKMS(FallbackConfig{
		Primary:             recovering,
		Mode:                RejectWrites,
		HealthCheckInterval: 50 * time.Millisecond, // fast health check for test
	})
	if err != nil {
		t.Fatalf("failed to create FallbackKMS: %v", err)
	}
	defer fk.Close()

	// Mark as unavailable to trigger health check
	fk.markUnavailable(fmt.Errorf("initial failure"))

	// Wait for health check to detect recovery
	time.Sleep(300 * time.Millisecond)

	// Should have recovered
	if !fk.IsAvailable() {
		t.Error("expected KMS to recover after health check")
	}
}

func TestFallbackKMSNoPrimary(t *testing.T) {
	_, err := NewFallbackKMS(FallbackConfig{
		Primary: nil,
		Mode:    RejectWrites,
	})
	if err == nil {
		t.Error("expected error when primary is nil")
	}
}

func TestFallbackKMSDefaultMode(t *testing.T) {
	mock := &mockKMS{
		plaintext: []byte("test-dek"),
		encrypted: []byte("encrypted"),
	}

	fk, err := NewFallbackKMS(FallbackConfig{
		Primary:             mock,
		Mode:                "", // empty, should default to reject_writes
		HealthCheckInterval: 1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("failed to create FallbackKMS: %v", err)
	}
	defer fk.Close()

	if fk.mode != RejectWrites {
		t.Errorf("expected default mode to be reject_writes, got %s", fk.mode)
	}
}

func TestFallbackKMSCacheIsolation(t *testing.T) {
	mock := &mockKMS{
		plaintext: []byte("test-dek-32-bytes-long-key!!"),
		encrypted: []byte("encrypted-dek-blob"),
		pubKey:    []byte("public-key"),
	}

	fk, err := NewFallbackKMS(FallbackConfig{
		Primary:             mock,
		Mode:                RejectWrites,
		HealthCheckInterval: 1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("failed to create FallbackKMS: %v", err)
	}
	defer fk.Close()

	ctx := context.Background()

	// Generate a key
	_, encrypted, err := fk.GenerateDataKey(ctx, "", 32)
	if err != nil {
		t.Fatalf("GenerateDataKey failed: %v", err)
	}

	// Look up from cache
	cached := fk.lookupCachedKey(encrypted)
	if cached == nil {
		t.Error("expected key to be cached")
	}

	// Verify the returned cached key is a copy (not the same slice)
	cached2 := fk.lookupCachedKey(encrypted)
	if &cached[0] == &cached2[0] {
		t.Error("cached key should be a copy, not the same slice")
	}
}
