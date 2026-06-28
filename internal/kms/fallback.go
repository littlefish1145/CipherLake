package kms

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// DegradationMode defines how the system behaves when the KMS is unavailable.
type DegradationMode string

const (
	// RejectWrites rejects all write operations when KMS is unavailable.
	// Read operations are allowed if the DEK can be decrypted locally.
	RejectWrites DegradationMode = "reject_writes"

	// ReadOnly allows read operations when KMS is unavailable,
	// using cached keys. Write operations are rejected.
	ReadOnly DegradationMode = "read_only"
)

// FallbackKMS wraps a primary KMSClient with fallback behavior for when
// the KMS becomes unavailable. It supports two degradation modes:
//   - reject_writes: reject write operations, allow reads with cached keys
//   - read_only: same as reject_writes (alias for clarity)
//
// When the KMS is unavailable, previously generated data keys are cached
// so that decrypt operations can still succeed. A background health check
// goroutine monitors KMS availability and auto-recovers when it comes back.
type FallbackKMS struct {
	primary     KMSClient
	mode        DegradationMode
	healthCheck time.Duration

	mu          sync.RWMutex
	available   bool
	cachedKeys  map[string][]byte // encrypted_key_hex -> plaintext_key
	lastFailure time.Time

	cancelCtx  context.Context
	cancelFunc context.CancelFunc
}

// FallbackConfig holds configuration for the FallbackKMS.
type FallbackConfig struct {
	// Primary is the underlying KMS client.
	Primary KMSClient
	// Mode is the degradation mode ("reject_writes" or "read_only").
	Mode DegradationMode
	// HealthCheckInterval is how often to check if the KMS has recovered.
	// Defaults to 10 seconds.
	HealthCheckInterval time.Duration
}

// NewFallbackKMS creates a new FallbackKMS wrapping the primary client.
func NewFallbackKMS(cfg FallbackConfig) (*FallbackKMS, error) {
	if cfg.Primary == nil {
		return nil, fmt.Errorf("kms/fallback: primary KMS client is required")
	}

	mode := cfg.Mode
	if mode == "" {
		mode = RejectWrites
	}

	healthCheck := cfg.HealthCheckInterval
	if healthCheck <= 0 {
		healthCheck = 10 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())

	fk := &FallbackKMS{
		primary:     cfg.Primary,
		mode:        mode,
		healthCheck: healthCheck,
		available:   true, // assume available initially
		cachedKeys:  make(map[string][]byte),
		cancelCtx:   ctx,
		cancelFunc:  cancel,
	}

	// Start health check goroutine
	go fk.healthCheckLoop()

	return fk, nil
}

// GenerateDataKey generates a new data key via the primary KMS.
// If the KMS is unavailable, it returns an error (writes are always rejected
// when KMS is down, since we cannot generate new keys without it).
func (f *FallbackKMS) GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error) {
	f.mu.RLock()
	available := f.available
	f.mu.RUnlock()

	if !available {
		f.logDegradedWarning("GenerateDataKey")
		return nil, nil, fmt.Errorf("kms/fallback: KMS unavailable, cannot generate new data key (mode: %s)", f.mode)
	}

	plaintext, encrypted, err = f.primary.GenerateDataKey(ctx, keyID, length)
	if err != nil {
		f.markUnavailable(err)
		return nil, nil, fmt.Errorf("kms/fallback: GenerateDataKey failed: %w", err)
	}

	// Cache the key for future decrypt operations if KMS goes down
	f.cacheKey(encrypted, plaintext)

	return plaintext, encrypted, nil
}

// DecryptDataKey decrypts an encrypted data key.
// If the KMS is unavailable, it attempts to use cached keys.
func (f *FallbackKMS) DecryptDataKey(ctx context.Context, keyID string, encrypted []byte) (plaintext []byte, err error) {
	f.mu.RLock()
	available := f.available
	f.mu.RUnlock()

	if available {
		plaintext, err = f.primary.DecryptDataKey(ctx, keyID, encrypted)
		if err != nil {
			f.markUnavailable(err)
			// Fall through to cache lookup
		} else {
			// Cache for future use
			f.cacheKey(encrypted, plaintext)
			return plaintext, nil
		}
	}

	// KMS unavailable or failed - try cache
	cachedPlaintext := f.lookupCachedKey(encrypted)
	if cachedPlaintext != nil {
		f.logDegradedWarning("DecryptDataKey (using cache)")
		return cachedPlaintext, nil
	}

	if !available {
		return nil, fmt.Errorf("kms/fallback: KMS unavailable and no cached key found (mode: %s)", f.mode)
	}
	return nil, fmt.Errorf("kms/fallback: DecryptDataKey failed and no cached key available: %w", err)
}

// GetPublicKey retrieves the public key from the primary KMS.
func (f *FallbackKMS) GetPublicKey(ctx context.Context, keyID string) (pub []byte, err error) {
	f.mu.RLock()
	available := f.available
	f.mu.RUnlock()

	if !available {
		f.logDegradedWarning("GetPublicKey")
		return nil, fmt.Errorf("kms/fallback: KMS unavailable, cannot get public key (mode: %s)", f.mode)
	}

	pub, err = f.primary.GetPublicKey(ctx, keyID)
	if err != nil {
		f.markUnavailable(err)
		return nil, fmt.Errorf("kms/fallback: GetPublicKey failed: %w", err)
	}

	return pub, nil
}

// Close stops the health check goroutine and closes the primary client.
func (f *FallbackKMS) Close() error {
	f.cancelFunc()
	return f.primary.Close()
}

// IsAvailable returns whether the primary KMS is currently available.
func (f *FallbackKMS) IsAvailable() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.available
}

// markUnavailable marks the primary KMS as unavailable.
func (f *FallbackKMS) markUnavailable(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.available {
		f.available = false
		f.lastFailure = time.Now()
		zap.L().Error("kms/fallback: KMS marked as unavailable",
			zap.Error(err),
			zap.String("mode", string(f.mode)))
	}
}

// markAvailable marks the primary KMS as available again.
func (f *FallbackKMS) markAvailable() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.available {
		f.available = true
		zap.L().Info("kms/fallback: KMS recovered and is now available")
	}
}

// cacheKey stores a plaintext key indexed by its encrypted form.
func (f *FallbackKMS) cacheKey(encrypted, plaintext []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := string(encrypted)
	cached := make([]byte, len(plaintext))
	copy(cached, plaintext)
	f.cachedKeys[key] = cached
}

// lookupCachedKey looks up a plaintext key by its encrypted form.
func (f *FallbackKMS) lookupCachedKey(encrypted []byte) []byte {
	f.mu.RLock()
	defer f.mu.RUnlock()

	key := string(encrypted)
	if plaintext, ok := f.cachedKeys[key]; ok {
		result := make([]byte, len(plaintext))
		copy(result, plaintext)
		return result
	}
	return nil
}

// logDegradedWarning logs a warning when operating in degraded mode.
func (f *FallbackKMS) logDegradedWarning(operation string) {
	zap.L().Warn("kms/fallback: operating in degraded mode",
		zap.String("operation", operation),
		zap.String("mode", string(f.mode)))
}

// healthCheckLoop periodically checks if the KMS has recovered.
func (f *FallbackKMS) healthCheckLoop() {
	ticker := time.NewTicker(f.healthCheck)
	defer ticker.Stop()

	for {
		select {
		case <-f.cancelCtx.Done():
			return
		case <-ticker.C:
			f.checkHealth()
		}
	}
}

// checkHealth attempts a lightweight KMS operation to check availability.
func (f *FallbackKMS) checkHealth() {
	f.mu.RLock()
	wasAvailable := f.available
	f.mu.RUnlock()

	// Only check if currently unavailable
	if wasAvailable {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try GetPublicKey as a lightweight health check
	_, err := f.primary.GetPublicKey(ctx, "")
	if err == nil {
		f.markAvailable()
	} else {
		zap.L().Debug("kms/fallback: health check still failing", zap.Error(err))
	}
}
