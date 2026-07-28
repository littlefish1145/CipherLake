package pluginloader

import (
	"context"
	"math"
	"sync"
	"time"

	"cipherlake/internal/config"
)

const (
	wasmPageSize = 64 * 1024
	maxWasmPages = 65536
)

// defaultResourceCap is the fallback when a config layer leaves a field at
// zero. These values are intentionally conservative for an untrusted plugin.
var defaultResourceCap = config.PluginLoaderResourceCap{
	MemoryMB:      512,
	FuelPerSec:    0, // 0 means unlimited (wazero v1.12 has no instruction fuel)
	CallTimeoutMS: 1000,
	IOBytesPerSec: 0, // 0 means unlimited at the host-import layer
	Concurrent:    16,
}

// EffectiveLimits is the resolved per-plugin resource envelope after applying
// the three-layer rule: min(global, tier, manifest).
type EffectiveLimits struct {
	MemoryPages   uint32
	FuelPerSec    int64
	CallTimeout   time.Duration
	IOBytesPerSec int64
	Concurrent    int
}

// ResourceLimiter resolves the three-layer resource model from config and
// manifest requests (spec §3.6).
type ResourceLimiter struct {
	limits config.PluginLoaderResourceLimits
}

// NewResourceLimiter creates a limiter from config. Missing fields are
// filled with defaultResourceCap before resolution.
func NewResourceLimiter(limits config.PluginLoaderResourceLimits) *ResourceLimiter {
	return &ResourceLimiter{limits: limits}
}

// GlobalMemoryPages returns the runtime-wide memory page cap derived from the
// global config. wazero applies this per runtime, so we set it to the largest
// allowed value any plugin may request.
func (rl *ResourceLimiter) GlobalMemoryPages() uint32 {
	g := normalizeCap(rl.limits.Global, defaultResourceCap)
	return mbToPages(g.MemoryMB)
}

// ForTier resolves the effective limits for a plugin loaded at the given
// trust tier with the given manifest resource request.
func (rl *ResourceLimiter) ForTier(tier int, manifest *ManifestResources) EffectiveLimits {
	global := normalizeCap(rl.limits.Global, defaultResourceCap)
	tierCap := global
	switch tier {
	case TierTrusted:
		tierCap = normalizeCap(rl.limits.Tier1, global)
	case TierCore:
		tierCap = normalizeCap(rl.limits.Tier2, global)
	default:
		tierCap = normalizeCap(rl.limits.Tier0, global)
	}

	el := EffectiveLimits{
		MemoryPages:   mbToPages(minInt(tierCap.MemoryMB, global.MemoryMB)),
		FuelPerSec:    minInt64(tierCap.FuelPerSec, global.FuelPerSec),
		CallTimeout:   minDuration(msToDuration(tierCap.CallTimeoutMS), msToDuration(global.CallTimeoutMS)),
		IOBytesPerSec: minInt64(tierCap.IOBytesPerSec, global.IOBytesPerSec),
		Concurrent:    minInt(tierCap.Concurrent, global.Concurrent),
	}

	if manifest != nil {
		if manifest.MemoryMB > 0 {
			el.MemoryPages = minUint32(el.MemoryPages, mbToPages(manifest.MemoryMB))
		}
		if manifest.FuelPerSec > 0 {
			el.FuelPerSec = minInt64(el.FuelPerSec, manifest.FuelPerSec)
		}
		if manifest.CallTimeout != "" {
			if d, err := time.ParseDuration(manifest.CallTimeout); err == nil && d > 0 {
				el.CallTimeout = minDuration(el.CallTimeout, d)
			}
		}
		if manifest.IOBytesPerSec > 0 {
			el.IOBytesPerSec = minInt64(el.IOBytesPerSec, manifest.IOBytesPerSec)
		}
		if manifest.Concurrent > 0 {
			el.Concurrent = minInt(el.Concurrent, manifest.Concurrent)
		}
	}

	if el.Concurrent <= 0 {
		el.Concurrent = global.Concurrent
	}
	if el.CallTimeout <= 0 {
		el.CallTimeout = msToDuration(global.CallTimeoutMS)
	}
	if el.MemoryPages == 0 {
		el.MemoryPages = mbToPages(global.MemoryMB)
	}
	return el
}

// normalizeCap fills zero-valued fields in cap with the corresponding value
// from fallback. A value of exactly zero is treated as "not configured".
func normalizeCap(cap, fallback config.PluginLoaderResourceCap) config.PluginLoaderResourceCap {
	if cap.MemoryMB == 0 {
		cap.MemoryMB = fallback.MemoryMB
	}
	if cap.FuelPerSec == 0 {
		cap.FuelPerSec = fallback.FuelPerSec
	}
	if cap.CallTimeoutMS == 0 {
		cap.CallTimeoutMS = fallback.CallTimeoutMS
	}
	if cap.IOBytesPerSec == 0 {
		cap.IOBytesPerSec = fallback.IOBytesPerSec
	}
	if cap.Concurrent == 0 {
		cap.Concurrent = fallback.Concurrent
	}
	return cap
}

func mbToPages(mb int) uint32 {
	if mb <= 0 {
		return 0
	}
	pages := uint64(mb) * 1024 * 1024 / wasmPageSize
	if pages > maxWasmPages {
		pages = maxWasmPages
	}
	return uint32(pages)
}

func msToDuration(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func minUint32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// TokenBucket is a simple rate limiter used for IO and fuel budgets. Tokens
// refill continuously at rate tokens per second up to a burst capacity.
// A Wait call blocks until the requested tokens are available or the context
// is cancelled. Zero rate means unlimited.
type TokenBucket struct {
	rate   float64 // tokens per second; 0 = unlimited
	burst  float64
	tokens float64
	last   time.Time
	mu     sync.Mutex
}

// NewTokenBucket creates a bucket with the given rate and burst. If rate is
// zero the bucket is unlimited.
func NewTokenBucket(rate int64, burst int64) *TokenBucket {
	b := &TokenBucket{
		rate:  float64(rate),
		burst: float64(burst),
	}
	if b.burst <= 0 {
		b.burst = b.rate
	}
	if b.burst < 1 {
		b.burst = 1
	}
	b.tokens = b.burst
	b.last = time.Now()
	return b
}

// Wait blocks until n tokens are available, deducts them, and returns. If the
// bucket is unlimited (rate == 0) it returns immediately. If the context is
// cancelled it returns ctx.Err() without deducting.
func (b *TokenBucket) Wait(ctx context.Context, n int64) error {
	if b == nil || b.rate == 0 {
		return nil
	}
	need := float64(n)
	if need <= 0 {
		return nil
	}

	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.last).Seconds()
		b.tokens = math.Min(b.tokens+elapsed*b.rate, b.burst)
		b.last = now
		if b.tokens >= need {
			b.tokens -= need
			b.mu.Unlock()
			return nil
		}
		deficit := need - b.tokens
		b.mu.Unlock()

		waitTime := time.Duration(math.Ceil(deficit/b.rate*1000)) * time.Millisecond
		if waitTime < time.Millisecond {
			waitTime = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitTime):
			// Recheck after refill.
		}
	}
}

// TryConsume attempts to deduct n tokens without blocking. It returns true if
// tokens were available.
func (b *TokenBucket) TryConsume(n int64) bool {
	if b == nil || b.rate == 0 {
		return true
	}
	need := float64(n)
	if need <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = math.Min(b.tokens+now.Sub(b.last).Seconds()*b.rate, b.burst)
	b.last = now
	if b.tokens >= need {
		b.tokens -= need
		return true
	}
	return false
}
