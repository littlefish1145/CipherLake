package pluginloader

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"cipherlake/internal/iam"
)

// mockPolicyEvaluator is a stub PolicyEvaluator for host import tests.
type mockPolicyEvaluator struct {
	result *iam.EvalResult
}

func (m *mockPolicyEvaluator) Evaluate(ctx *iam.EvalContext) *iam.EvalResult {
	return m.result
}

// mockDataKeyGenerator is a stub DataKeyGenerator for host import tests.
type mockDataKeyGenerator struct {
	encrypted []byte
	err       error
}

func (m *mockDataKeyGenerator) GenerateDataKey(ctx context.Context, keyID string, length int) ([]byte, []byte, error) {
	return nil, m.encrypted, m.err
}

func (m *mockDataKeyGenerator) GetPublicKey(ctx context.Context, keyID string) ([]byte, error) {
	return nil, nil
}

func TestHostImports_IAMPolicyEvaluate_Tier2Allowed(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetPolicyEvaluator(&mockPolicyEvaluator{
		result: &iam.EvalResult{
			Decision:   iam.DecisionAllow,
			MatchedBy:  "stmt-1",
			PolicyType: "identity",
			Details:    "allowed",
		},
	})
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("iam-plugin", TierCore, []string{"iam:policy:evaluate"}, neverExpires())
	require.NoError(t, err)

	req, _ := json.Marshal(map[string]string{
		"principal": "arn:cipherlake:iam:::user/alice",
		"action":    "s3:GetObject",
		"resource":  "arn:cipherlake:s3:::bucket/obj",
	})
	const base uint32 = 64
	writeTestMemory(t, mem, base, req)

	h.iamPolicyEvaluate(ctx, mod, uint64(tok),
		base, uint32(len(req)),
		base+512, 256,
		base+256,
		base+260,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+256))
	respLen := readTestUint32(t, mem, base+260)
	respBytes, _ := mem.Read(base+512, respLen)
	var resp struct {
		Decision string `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(respBytes, &resp))
	require.Equal(t, "Allow", resp.Decision)
}

func TestHostImports_IAMPolicyEvaluate_Tier0Denied(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetPolicyEvaluator(&mockPolicyEvaluator{result: &iam.EvalResult{Decision: iam.DecisionDeny}})
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("iam-plugin", TierUntrusted, []string{"iam:policy:evaluate"}, neverExpires())
	require.NoError(t, err)

	req := []byte("{}")
	const base uint32 = 64
	writeTestMemory(t, mem, base, req)
	h.iamPolicyEvaluate(ctx, mod, uint64(tok),
		base, uint32(len(req)),
		base+512, 256,
		base+256,
		base+260,
	)
	require.Equal(t, statusUnauthorized, readTestUint32(t, mem, base+256))
}

func TestHostImports_KMSDataKeyGenerate_Tier2Allowed(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetDataKeyGenerator(&mockDataKeyGenerator{encrypted: []byte("encrypted-dek-bytes")})
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("kms-plugin", TierCore, []string{"kms:datakey:generate"}, neverExpires())
	require.NoError(t, err)

	req, _ := json.Marshal(map[string]interface{}{"length": 32})
	const base uint32 = 64
	writeTestMemory(t, mem, base, req)
	h.kmsDataKeyGenerate(ctx, mod, uint64(tok),
		base, uint32(len(req)),
		base+512, 256,
		base+256,
		base+260,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+256))
	respLen := readTestUint32(t, mem, base+260)
	respBytes, _ := mem.Read(base+512, respLen)
	var resp struct {
		EncryptedDEK string `json:"encrypted_dek"`
	}
	require.NoError(t, json.Unmarshal(respBytes, &resp))
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("encrypted-dek-bytes")), resp.EncryptedDEK)
}

func TestHostImports_KMSDataKeyGenerate_Tier0Denied(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetDataKeyGenerator(&mockDataKeyGenerator{encrypted: []byte("x")})
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("kms-plugin", TierUntrusted, []string{"kms:datakey:generate"}, neverExpires())
	require.NoError(t, err)

	req := []byte("{}")
	const base uint32 = 64
	writeTestMemory(t, mem, base, req)
	h.kmsDataKeyGenerate(ctx, mod, uint64(tok),
		base, uint32(len(req)),
		base+512, 256,
		base+256,
		base+260,
	)
	require.Equal(t, statusUnauthorized, readTestUint32(t, mem, base+256))
}

func TestHostImports_CryptoAuditSign_Tier2Allowed(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	signer, err := NewECDSAAuditSigner(t.TempDir()+"/audit", "audit-key-1")
	require.NoError(t, err)

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetAuditSigner(signer)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("crypto-plugin", TierCore, []string{"crypto:audit:sign"}, neverExpires())
	require.NoError(t, err)

	payload := []byte("audit-payload")
	const base uint32 = 64
	writeTestMemory(t, mem, base, payload)
	h.cryptoAuditSign(ctx, mod, uint64(tok),
		base, uint32(len(payload)),
		base+512, 256,
		base+256,
		base+260,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+256))
	sigLen := readTestUint32(t, mem, base+260)
	require.Greater(t, sigLen, uint32(0))
}

func TestHostImports_CryptoAuditSign_Tier0Denied(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	signer, err := NewECDSAAuditSigner(t.TempDir()+"/audit", "audit-key-1")
	require.NoError(t, err)

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetAuditSigner(signer)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("crypto-plugin", TierUntrusted, []string{"crypto:audit:sign"}, neverExpires())
	require.NoError(t, err)

	payload := []byte("audit-payload")
	const base uint32 = 64
	writeTestMemory(t, mem, base, payload)
	h.cryptoAuditSign(ctx, mod, uint64(tok),
		base, uint32(len(payload)),
		base+512, 256,
		base+256,
		base+260,
	)
	require.Equal(t, statusUnauthorized, readTestUint32(t, mem, base+256))
}

func TestHostImports_GatewayHookRegisterForOther_Tier2Allowed(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetHookRegistrar(l)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("hook-caller", TierCore, []string{"gateway:hook:register_for_other"}, neverExpires())
	require.NoError(t, err)

	req, _ := json.Marshal(map[string]string{
		"operation": "s3:GetObject",
		"hook_type": "before",
		"handler":   "on_hook",
	})
	const base uint32 = 64
	writeTestMemory(t, mem, base, req)
	h.gatewayHookRegisterForOther(ctx, mod, uint64(tok),
		base, uint32(len(req)),
		base+256,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+256))

	hooks := l.hooks.forOperation("s3:GetObject")
	require.Len(t, hooks, 1)
	require.Equal(t, "hook-caller", hooks[0].PluginName)
}

func TestHostImports_GatewayHookRegisterForOther_Tier0Denied(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetHookRegistrar(l)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("hook-caller", TierUntrusted, []string{"gateway:hook:register_for_other"}, neverExpires())
	require.NoError(t, err)

	req, _ := json.Marshal(map[string]string{
		"operation": "s3:GetObject",
		"hook_type": "before",
		"handler":   "on_hook",
	})
	const base uint32 = 64
	writeTestMemory(t, mem, base, req)
	h.gatewayHookRegisterForOther(ctx, mod, uint64(tok),
		base, uint32(len(req)),
		base+256,
	)
	require.Equal(t, statusUnauthorized, readTestUint32(t, mem, base+256))
}
