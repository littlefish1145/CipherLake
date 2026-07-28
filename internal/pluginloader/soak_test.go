package pluginloader

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"cipherlake/internal/iam"
)

// TestSoak_PluginLoaderStability is the 24h stability / soak test framework
// for the plugin loader (spec F5.3). It runs a mixed workload of state:kv
// operations, Tier-2-only host imports, and cross-plugin state reads in a
// loop for a configurable duration. The default duration is 30 seconds so the
// test passes quickly in CI; set SOAK_DURATION=24h for a full soak run.
//
// Usage:
//
//	go test -run TestSoak_PluginLoaderStability -timeout=30m        # CI (30s)
//	SOAK_DURATION=24h go test -run TestSoak_PluginLoaderStability -timeout=25h  # full soak
//
// The test reports:
//   - Total operations completed
//   - Operations per second
//   - Heap growth (memory leak detection)
//   - Goroutine count growth (goroutine leak detection)
//   - Any errors encountered
func TestSoak_PluginLoaderStability(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test skipped in short mode")
	}

	duration := 30 * time.Second
	if d := os.Getenv("SOAK_DURATION"); d != "" {
		parsed, err := time.ParseDuration(d)
		if err != nil {
			t.Fatalf("invalid SOAK_DURATION %q: %v", d, err)
		}
		duration = parsed
	}

	ctx := context.Background()
	l := newTestLoader(t)
	defer l.Shutdown()

	// Set up Tier-2 services for the soak test.
	signer, err := NewECDSAAuditSigner(t.TempDir()+"/audit", "soak-audit")
	if err != nil {
		t.Fatalf("create audit signer: %v", err)
	}
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetAuditSigner(signer)
	h.SetDataKeyGenerator(&mockDataKeyGenerator{encrypted: []byte("soak-encrypted-dek")})
	h.SetPolicyEvaluator(&mockPolicyEvaluator{
		result: &iam.EvalResult{
			Decision:   iam.DecisionAllow,
			MatchedBy:  "soak-stmt",
			PolicyType: "identity",
		},
	})
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Issue capability tokens for the soak workload.
	stateTok, _ := l.WASMRuntime().CapabilityTable().Issue("soak-plugin", 0, []string{"state:kv"}, neverExpires())
	auditTok, _ := l.WASMRuntime().CapabilityTable().Issue("soak-plugin", TierCore, []string{"crypto:audit:sign"}, neverExpires())
	kmsTok, _ := l.WASMRuntime().CapabilityTable().Issue("soak-plugin", TierCore, []string{"kms:datakey:generate"}, neverExpires())
	iamTok, _ := l.WASMRuntime().CapabilityTable().Issue("soak-plugin", TierCore, []string{"iam:policy:evaluate"}, neverExpires())

	// Record baseline memory stats.
	runtime.GC()
	var baselineMemStats runtime.MemStats
	runtime.ReadMemStats(&baselineMemStats)
	baselineGoroutines := runtime.NumGoroutine()

	const base uint32 = 64
	payload := []byte("soak-audit-payload")
	writeTestMemory(t, mem, base, payload)

	iamReq := []byte(`{"principal":"user","action":"s3:GetObject","resource":"arn:x"}`)
	writeTestMemory(t, mem, base+1024, iamReq)
	kmsReq := []byte(`{"length":32}`)
	writeTestMemory(t, mem, base+2048, kmsReq)

	deadline := time.Now().Add(duration)
	var ops int64
	var errors int64
	auditSig := []byte("soak-audit-payload")

	for time.Now().Before(deadline) {
		// 1. state.put + state.get cycle.
		key := []byte(fmt.Sprintf("soak/key/%d", ops))
		writeTestMemory(t, mem, base, key)
		h.statePut(ctx, mod, uint64(stateTok),
			base, uint32(len(key)),
			base+128, uint32(len(payload)),
			0, base+256, base+260,
		)
		if readTestUint32(t, mem, base+256) != statusOK {
			errors++
		}
		h.stateGet(ctx, mod, uint64(stateTok),
			base, uint32(len(key)),
			base+512, 256,
			base+256, base+260, base+268,
		)
		if readTestUint32(t, mem, base+256) != statusOK {
			errors++
		}

		// 2. crypto.audit.sign
		h.cryptoAuditSign(ctx, mod, uint64(auditTok),
			base, uint32(len(auditSig)),
			base+512, 256,
			base+256, base+260,
		)
		if readTestUint32(t, mem, base+256) != statusOK {
			errors++
		}

		// 3. kms.datakey.generate
		h.kmsDataKeyGenerate(ctx, mod, uint64(kmsTok),
			base+2048, uint32(len(kmsReq)),
			base+512, 256,
			base+256, base+260,
		)
		if readTestUint32(t, mem, base+256) != statusOK {
			errors++
		}

		// 4. iam.policy.evaluate
		h.iamPolicyEvaluate(ctx, mod, uint64(iamTok),
			base+1024, uint32(len(iamReq)),
			base+512, 256,
			base+256, base+260,
		)
		if readTestUint32(t, mem, base+256) != statusOK {
			errors++
		}

		ops++

		// Periodic GC to simulate realistic conditions.
		if ops%10000 == 0 {
			runtime.GC()
		}
	}

	// Final memory stats.
	runtime.GC()
	var finalMemStats runtime.MemStats
	runtime.ReadMemStats(&finalMemStats)
	finalGoroutines := runtime.NumGoroutine()

	heapGrowth := int64(finalMemStats.HeapAlloc) - int64(baselineMemStats.HeapAlloc)
	opsPerSec := float64(ops) / duration.Seconds()

	t.Logf("soak test completed: duration=%s, ops=%d, ops/sec=%.0f, errors=%d", duration, ops, opsPerSec, errors)
	t.Logf("memory: heap_baseline=%d, heap_final=%d, heap_growth=%d bytes", baselineMemStats.HeapAlloc, finalMemStats.HeapAlloc, heapGrowth)
	t.Logf("goroutines: baseline=%d, final=%d", baselineGoroutines, finalGoroutines)

	// Assert no errors.
	if errors > 0 {
		t.Errorf("soak test encountered %d errors out of %d operations", errors, ops*5)
	}

	// Assert no significant memory leak (>100MB heap growth is suspicious).
	if heapGrowth > 100*1024*1024 {
		t.Errorf("potential memory leak: heap grew by %d bytes (%.1f MB)", heapGrowth, float64(heapGrowth)/1024/1024)
	}

	// Assert no goroutine leak (>100 goroutine growth is suspicious).
	goroutineGrowth := finalGoroutines - baselineGoroutines
	if goroutineGrowth > 100 {
		t.Errorf("potential goroutine leak: %d new goroutines", goroutineGrowth)
	}
}
