package pluginloader

import (
	"context"
	"testing"
	"time"

	"cipherlake/internal/config"

	"github.com/stretchr/testify/require"
)

func TestResourceLimiter_Defaults(t *testing.T) {
	rl := NewResourceLimiter(config.PluginLoaderResourceLimits{})
	el := rl.ForTier(TierUntrusted, nil)
	require.Equal(t, 16, el.Concurrent)
	require.Equal(t, 1*time.Second, el.CallTimeout)
	require.Equal(t, uint32(512*1024*1024/wasmPageSize), el.MemoryPages)
}

func TestResourceLimiter_ThreeLayerMin(t *testing.T) {
	rl := NewResourceLimiter(config.PluginLoaderResourceLimits{
		Global: config.PluginLoaderResourceCap{MemoryMB: 512, Concurrent: 16, CallTimeoutMS: 1000},
		Tier0:  config.PluginLoaderResourceCap{MemoryMB: 128, Concurrent: 4, CallTimeoutMS: 500},
		Tier1:  config.PluginLoaderResourceCap{MemoryMB: 256, Concurrent: 8, CallTimeoutMS: 2000},
	})

	// Tier 0 plugin gets Tier0 cap (stricter than global).
	el0 := rl.ForTier(TierUntrusted, nil)
	require.Equal(t, 4, el0.Concurrent)
	require.Equal(t, 500*time.Millisecond, el0.CallTimeout)
	require.Equal(t, uint32(128*1024*1024/wasmPageSize), el0.MemoryPages)

	// Tier 1 plugin gets Tier1 cap (memory stricter, call timeout less strict
	// than global so global wins).
	el1 := rl.ForTier(TierTrusted, nil)
	require.Equal(t, 8, el1.Concurrent)
	require.Equal(t, 1*time.Second, el1.CallTimeout) // min(2000ms, 1000ms)
	require.Equal(t, uint32(256*1024*1024/wasmPageSize), el1.MemoryPages)
}

func TestResourceLimiter_ManifestOverrides(t *testing.T) {
	rl := NewResourceLimiter(config.PluginLoaderResourceLimits{
		Global: config.PluginLoaderResourceCap{MemoryMB: 512, Concurrent: 16, CallTimeoutMS: 1000},
		Tier1:  config.PluginLoaderResourceCap{MemoryMB: 256, Concurrent: 8, CallTimeoutMS: 2000},
	})
	manifest := &ManifestResources{
		MemoryMB:    64,
		Concurrent:  2,
		CallTimeout: "100ms",
	}
	el := rl.ForTier(TierTrusted, manifest)
	require.Equal(t, 2, el.Concurrent)
	require.Equal(t, 100*time.Millisecond, el.CallTimeout)
	require.Equal(t, uint32(64*1024*1024/wasmPageSize), el.MemoryPages)
}

func TestResourceLimiter_GlobalMemoryPages(t *testing.T) {
	rl := NewResourceLimiter(config.PluginLoaderResourceLimits{
		Global: config.PluginLoaderResourceCap{MemoryMB: 128},
	})
	require.Equal(t, uint32(128*1024*1024/wasmPageSize), rl.GlobalMemoryPages())
}

func TestTokenBucket_Unlimited(t *testing.T) {
	b := NewTokenBucket(0, 0)
	require.NoError(t, b.Wait(context.Background(), 1_000_000))
}

func TestTokenBucket_TryConsume(t *testing.T) {
	b := NewTokenBucket(1000, 10)
	// Burst is 10, so first 10 tokens succeed.
	require.True(t, b.TryConsume(10))
	require.False(t, b.TryConsume(1))
}

func TestTokenBucket_WaitRefills(t *testing.T) {
	b := NewTokenBucket(1000, 1)
	require.True(t, b.TryConsume(1))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// At 1000 tokens/sec, one token refills in ~1ms; should succeed quickly.
	require.NoError(t, b.Wait(ctx, 1))
}

func TestTokenBucket_ContextCancel(t *testing.T) {
	b := NewTokenBucket(1, 1)
	require.True(t, b.TryConsume(1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := b.Wait(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
}
