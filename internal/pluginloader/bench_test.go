package pluginloader

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"cipherlake/internal/iam"
)

// BenchmarkStatePut measures state.put throughput through the host-import ABI.
// It exercises the full path: capability authorization → Raft replication →
// BoltDB write. This is the P5-2 end-to-end performance baseline (spec §3.14,
// F5.2).
func BenchmarkStatePut(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("bench-plugin", 0, []string{"state:kv"}, neverExpires())

	const base uint32 = 64
	value := []byte(`{"data":"benchmark-value"}`)
	writeTestMemory(b, mem, base+128, value)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := []byte(fmt.Sprintf("bench/put/%d", i))
		writeTestMemory(b, mem, base, key)
		h.statePut(ctx, mod, uint64(tok),
			base, uint32(len(key)),
			base+128, uint32(len(value)),
			0,
			base+256,
			base+260,
		)
		if readTestUint32(b, mem, base+256) != statusOK {
			b.Fatalf("state.put failed at iteration %d", i)
		}
	}
}

// BenchmarkStateGet measures state.get throughput through the host-import ABI.
func BenchmarkStateGet(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("bench-plugin", 0, []string{"state:kv"}, neverExpires())

	const base uint32 = 64
	key := []byte("bench/get/key")
	value := []byte(`{"data":"benchmark-value"}`)
	writeTestMemory(b, mem, base, key)
	writeTestMemory(b, mem, base+128, value)
	h.statePut(ctx, mod, uint64(tok),
		base, uint32(len(key)),
		base+128, uint32(len(value)),
		0, base+256, base+260,
	)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.stateGet(ctx, mod, uint64(tok),
			base, uint32(len(key)),
			base+512, 256,
			base+256,
			base+260,
			base+268,
		)
		if readTestUint32(b, mem, base+256) != statusOK {
			b.Fatalf("state.get failed at iteration %d", i)
		}
	}
}

// BenchmarkStateCAS measures state.cas (compare-and-swap) throughput.
func BenchmarkStateCAS(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("bench-plugin", 0, []string{"state:kv"}, neverExpires())

	const base uint32 = 64
	key := []byte("bench/cas/counter")
	writeTestMemory(b, mem, base, key)

	// Initialize at version 0.
	val := []byte("0")
	writeTestMemory(b, mem, base+128, val)
	h.statePut(ctx, mod, uint64(tok),
		base, uint32(len(key)),
		base+128, uint32(len(val)),
		0, base+256, base+260,
	)
	currentVersion := readTestUint64(b, mem, base+260)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		newVal := []byte(fmt.Sprintf("%d", i+1))
		writeTestMemory(b, mem, base+128, newVal)
		h.stateCas(ctx, mod, uint64(tok),
			base, uint32(len(key)),
			base+128, uint32(len(newVal)),
			currentVersion,
			base+256,
			base+260,
		)
		if readTestUint32(b, mem, base+256) != statusOK {
			b.Fatalf("state.cas failed at iteration %d", i)
		}
		currentVersion = readTestUint64(b, mem, base+260)
	}
}

// BenchmarkStateBatch measures state.batch throughput with 10 put operations.
func BenchmarkStateBatch(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("bench-plugin", 0, []string{"state:kv"}, neverExpires())

	const base uint32 = 64
	ops := make([]stateBatchOp, 10)
	for i := range ops {
		ops[i] = stateBatchOp{
			Op:    "put",
			Key:   fmt.Sprintf("bench/batch/%d", i),
			Value: json.RawMessage(`"val"`),
		}
	}
	opsJSON, _ := json.Marshal(ops)
	writeTestMemory(b, mem, base, opsJSON)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.stateBatch(ctx, mod, uint64(tok),
			base, uint32(len(opsJSON)),
			base+512,
			base+1024,
			512,
			base+516,
		)
		if readTestUint32(b, mem, base+512) != statusOK {
			b.Fatalf("state.batch failed at iteration %d", i)
		}
	}
}

// BenchmarkIAMPolicyEvaluate measures the Tier-2-only iam.policy.evaluate
// host import throughput (spec §3.7 A4, P5-5, F5.2).
func BenchmarkIAMPolicyEvaluate(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetPolicyEvaluator(&mockPolicyEvaluator{
		result: &iam.EvalResult{
			Decision:   iam.DecisionAllow,
			MatchedBy:  "bench-stmt",
			PolicyType: "identity",
		},
	})
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("iam-bench", TierCore, []string{"iam:policy:evaluate"}, neverExpires())

	req, _ := json.Marshal(map[string]string{
		"principal": "arn:cipherlake:iam:::user/bench",
		"action":    "s3:GetObject",
		"resource":  "arn:cipherlake:s3:::bench/obj",
	})
	const base uint32 = 64
	writeTestMemory(b, mem, base, req)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.iamPolicyEvaluate(ctx, mod, uint64(tok),
			base, uint32(len(req)),
			base+512, 256,
			base+256,
			base+260,
		)
		if readTestUint32(b, mem, base+256) != statusOK {
			b.Fatalf("iam.policy.evaluate failed at iteration %d", i)
		}
	}
}

// BenchmarkCrossPluginStateGet measures cross-plugin state.get throughput
// (spec §3.14 A13, P5-3, F5.2).
func BenchmarkCrossPluginStateGet(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Install plugin-a with a grant.
	aManifest := manifestBytesWithGrants("plugin-a", []ManifestGrant{
		{Plugin: "plugin-b", Keys: []string{"shared/*"}},
	})
	if err := l.InstallPlugin(ctx, mustParseManifest(aManifest), minimalPluginBytes, nil, ""); err != nil {
		b.Fatalf("install plugin-a: %v", err)
	}
	bManifest := manifestBytesWithGrants("plugin-b", nil)
	if err := l.InstallPlugin(ctx, mustParseManifest(bManifest), minimalPluginBytes, nil, ""); err != nil {
		b.Fatalf("install plugin-b: %v", err)
	}

	val := []byte("cross-plugin-value")
	if _, err := l.StatePut("plugin-a", "shared/bench", val, 0); err != nil {
		b.Fatalf("state.put: %v", err)
	}

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("plugin-b", 0, []string{"state:kv", "state:cross_plugin:plugin-a"}, neverExpires())

	const base uint32 = 64
	key := []byte("plugin-a:shared/bench")
	writeTestMemory(b, mem, base, key)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.stateGet(ctx, mod, uint64(tok),
			base, uint32(len(key)),
			base+512, 256,
			base+256,
			base+260,
			base+268,
		)
		if readTestUint32(b, mem, base+256) != statusOK {
			b.Fatalf("cross-plugin state.get failed at iteration %d", i)
		}
	}
}

// BenchmarkKMSDataKeyGenerate measures kms.datakey.generate throughput.
func BenchmarkKMSDataKeyGenerate(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetDataKeyGenerator(&mockDataKeyGenerator{encrypted: []byte("encrypted-dek-benchmark")})
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("kms-bench", TierCore, []string{"kms:datakey:generate"}, neverExpires())

	req, _ := json.Marshal(map[string]interface{}{"length": 32})
	const base uint32 = 64
	writeTestMemory(b, mem, base, req)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.kmsDataKeyGenerate(ctx, mod, uint64(tok),
			base, uint32(len(req)),
			base+512, 256,
			base+256,
			base+260,
		)
		if readTestUint32(b, mem, base+256) != statusOK {
			b.Fatalf("kms.datakey.generate failed at iteration %d", i)
		}
	}
}

// BenchmarkCryptoAuditSign measures crypto.audit.sign throughput.
func BenchmarkCryptoAuditSign(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	signer, err := NewECDSAAuditSigner(b.TempDir()+"/audit", "bench-key")
	if err != nil {
		b.Fatalf("create audit signer: %v", err)
	}

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetAuditSigner(signer)
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("crypto-bench", TierCore, []string{"crypto:audit:sign"}, neverExpires())

	payload := []byte("benchmark-audit-payload")
	const base uint32 = 64
	writeTestMemory(b, mem, base, payload)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.cryptoAuditSign(ctx, mod, uint64(tok),
			base, uint32(len(payload)),
			base+512, 256,
			base+256,
			base+260,
		)
		if readTestUint32(b, mem, base+256) != statusOK {
			b.Fatalf("crypto.audit.sign failed at iteration %d", i)
		}
	}
}

// BenchmarkGatewayHookRegister measures gateway.hook.register-for-other
// throughput.
func BenchmarkGatewayHookRegister(b *testing.B) {
	l := newTestLoader(b)
	defer l.Shutdown()

	ctx := context.Background()
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetHookRegistrar(l)
	mod := newTestMemoryModule(b, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, _ := l.WASMRuntime().CapabilityTable().Issue("hook-bench", TierCore, []string{"gateway:hook:register_for_other"}, neverExpires())

	req, _ := json.Marshal(map[string]string{
		"operation": "s3:GetObject",
		"hook_type": "before",
		"handler":   "on_hook",
	})
	const base uint32 = 64
	writeTestMemory(b, mem, base, req)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.gatewayHookRegisterForOther(ctx, mod, uint64(tok),
			base, uint32(len(req)),
			base+256,
		)
		if readTestUint32(b, mem, base+256) != statusOK {
			b.Fatalf("gateway.hook.register-for-other failed at iteration %d", i)
		}
	}
}
