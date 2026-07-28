package pluginloader

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// minimalWASM is a hand-built WASM module exporting the three mandatory
// entry points (wasm_init, on_hook, on_request) and nothing else. Each
// function returns i32.const 0 (status OK per spec §4).
//
// WAT equivalent:
//   (module
//     (func (export "wasm_init")  (param i64)             (result i32) i32.const 0)
//     (func (export "on_hook")    (param i64 i32 i64)     (result i32) i32.const 0)
//     (func (export "on_request") (param i64 i32 i64)     (result i32) i32.const 0)
//   )
const minimalWASMHex = "0061736d01000000" + // magic + version
	"010d0260017e017f60037e7f7e017f" + // type section: 2 types
	"030403000101" + // func section: 3 funcs (types 0,1,1)
	"072403097761736d5f696e69740000076f6e5f686f6f6b00010a6f6e5f726571756573740002" + // export section
	"0a1003040041000b040041000b040041000b" // code section

// minimalWASMMissingInit is a WASM module missing wasm_init. Used to
// verify entry-point validation rejects incomplete modules.
// Exports only on_hook and on_request, both using type (i64,i32,i64)->i32.
const minimalWASMMissingInitHex = "0061736d01000000" + // magic + version
	"01080160037e7f7e017f" + // type section: 1 type (i64,i32,i64)->i32
	"0303020000" + // func section: 2 funcs, both type 0
	"071802076f6e5f686f6f6b00000a6f6e5f726571756573740001" + // export: on_hook idx=0, on_request idx=1
	"0a0b02040041000b040041000b" // code section: 2 funcs returning i32.const 0

func mustHexDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("failed to decode hex fixture: %v", err)
	}
	return b
}

// TestParseManifest_Valid covers the happy-path manifest from spec §6.
func TestParseManifest_Valid(t *testing.T) {
	raw := []byte(`{
		"manifest_version": "1.0",
		"api_version": "^1.0.0",
		"name": "mcp-memory-server",
		"version": "0.1.0",
		"author": "you@example.com",
		"description": "MCP server",
		"trust_tier_requested": 1,
		"capabilities": [
			"http:route:/mcp/*",
			"vector:search:memories/*",
			"state:kv"
		],
		"hooks": [
			{"type": "around", "op": "s3:GetObject", "handler": "on_hook", "priority": 50, "on_failure": "log_and_allow"}
		],
		"routes": [
			{"prefix": "/mcp/*", "handler": "on_request", "sse_sessions": true}
		],
		"depends_on": []
	}`)
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("expected valid manifest, got err: %v", err)
	}
	if m.Name != "mcp-memory-server" {
		t.Errorf("name = %q", m.Name)
	}
	if m.TrustTierRequested != 1 {
		t.Errorf("trust_tier_requested = %d", m.TrustTierRequested)
	}
	if len(m.Capabilities) != 3 {
		t.Errorf("capabilities len = %d", len(m.Capabilities))
	}
	if len(m.Hooks) != 1 {
		t.Fatalf("hooks len = %d", len(m.Hooks))
	}
	if m.Hooks[0].Type != "around" {
		t.Errorf("hook type = %q", m.Hooks[0].Type)
	}
}

func TestParseManifest_Errors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"bad manifest_version", `{"manifest_version":"2.0","api_version":"^1.0.0","name":"abc","version":"1.0.0"}`},
		{"missing api_version", `{"manifest_version":"1.0","name":"abc","version":"1.0.0"}`},
		{"api_version range too narrow", `{"manifest_version":"1.0","api_version":">=2.0.0","name":"abc","version":"1.0.0"}`},
		{"bad name (uppercase)", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"ABC","version":"1.0.0"}`},
		{"bad name (too short)", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"ab","version":"1.0.0"}`},
		{"bad version (not semver)", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"abc","version":"xyz"}`},
		{"trust_tier out of range", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"abc","version":"1.0.0","trust_tier_requested":3}`},
		{"unknown capability namespace", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"abc","version":"1.0.0","capabilities":["bogus:thing"]}`},
		{"bad hook type", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"abc","version":"1.0.0","hooks":[{"type":"weird","op":"s3:GetObject","handler":"on_hook"}]}`},
		{"hook op not s3", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"abc","version":"1.0.0","hooks":[{"type":"before","op":"foo:Bar","handler":"on_hook"}]}`},
		{"hook priority out of range", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"abc","version":"1.0.0","hooks":[{"type":"before","op":"s3:GetObject","handler":"on_hook","priority":150}]}`},
		{"route missing /", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"abc","version":"1.0.0","routes":[{"prefix":"mcp","handler":"on_request"}]}`},
		{"route bare reserved /", `{"manifest_version":"1.0","api_version":"^1.0.0","name":"abc","version":"1.0.0","routes":[{"prefix":"/","handler":"on_request"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !IsErrManifestInvalid(err) {
				t.Logf("got err: %v", err)
			}
		})
	}
}

// TestCapabilityToken_IssueRevoke verifies token issuance, lookup, and
// revocation semantics (spec §3.3 A6).
func TestCapabilityToken_IssueRevoke(t *testing.T) {
	tab := NewCapabilityTable()
	tok, err := tab.Issue("plugin-a", 1, []string{"state:kv", "vector:search:mems/*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if tok == 0 {
		t.Error("token should not be 0 (sentinel)")
	}
	c := tab.Lookup(tok)
	if c == nil {
		t.Fatal("lookup returned nil for live token")
	}
	if c.PluginName != "plugin-a" {
		t.Errorf("plugin = %q", c.PluginName)
	}
	if c.TrustTier != 1 {
		t.Errorf("tier = %d", c.TrustTier)
	}
	tab.Revoke(tok)
	if tab.Lookup(tok) != nil {
		t.Error("lookup after revoke returned non-nil")
	}
}

// TestCapabilityToken_RevokeAllForPlugin verifies that all tokens for a
// plugin are revoked in one call (used during uninstall).
func TestCapabilityToken_RevokeAllForPlugin(t *testing.T) {
	tab := NewCapabilityTable()
	t1, _ := tab.Issue("plugin-a", 0, []string{"state:kv"}, neverExpires())
	t2, _ := tab.Issue("plugin-a", 0, []string{"state:kv"}, neverExpires())
	t3, _ := tab.Issue("plugin-b", 0, []string{"state:kv"}, neverExpires())

	n := tab.RevokeAllForPlugin("plugin-a")
	if n != 2 {
		t.Errorf("revoked count = %d, want 2", n)
	}
	if tab.Lookup(t1) != nil {
		t.Error("t1 still valid after RevokeAllForPlugin")
	}
	if tab.Lookup(t2) != nil {
		t.Error("t2 still valid")
	}
	if tab.Lookup(t3) == nil {
		t.Error("t3 should still be valid (different plugin)")
	}
}

// TestCapability_ScopeMatching covers the glob-based scope match used
// to authorize scoped capabilities like storage:get:bucket/prefix/*.
func TestCapability_ScopeMatching(t *testing.T) {
	c := &Capability{
		Capabilities: []string{
			"storage:get:memories/*",
			"state:kv",
			"vector:search:mems",
		},
	}
	cases := []struct {
		requested string
		want      bool
	}{
		{"storage:get:memories/sessions/abc", true},
		{"storage:get:memories", false}, // wildcard requires at least the prefix
		{"storage:get:other/x", false},
		{"state:kv", true},
		{"state:kv:extra", false},
		{"vector:search:mems", true},
		{"vector:search:mems/x", false},
	}
	for _, tc := range cases {
		got := c.HasCapability(tc.requested)
		if got != tc.want {
			t.Errorf("HasCapability(%q) = %v, want %v", tc.requested, got, tc.want)
		}
	}
}

// TestCapabilityTable_Authorize_Tier2 verifies Tier-2-only capabilities
// (spec §3.7 A4) are rejected for Tier 0/1 tokens.
func TestCapabilityTable_Authorize_Tier2(t *testing.T) {
	tab := NewCapabilityTable()
	tok0, _ := tab.Issue("p0", 0, []string{"iam:policy:evaluate"}, neverExpires())
	tok2, _ := tab.Issue("p2", 2, []string{"iam:policy:evaluate"}, neverExpires())

	if err := tab.Authorize(tok0, "iam:policy:evaluate"); err == nil {
		t.Error("Tier 0 token should not be allowed Tier 2 capability")
	}
	if err := tab.Authorize(tok2, "iam:policy:evaluate"); err != nil {
		t.Errorf("Tier 2 token should be allowed: %v", err)
	}
}

// TestManifest_Validate_Tier2CapabilityRequiresTier2Request verifies that
// declaring a Tier-2-only capability without requesting Tier 2 is rejected
// at manifest parse time (spec §3.7 A4, P5-5).
func TestManifest_Validate_Tier2CapabilityRequiresTier2Request(t *testing.T) {
	m := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         HostAPICompatRange,
		Name:               "tier2-cap-demo",
		Version:            "1.0.0",
		TrustTierRequested: TierTrusted,
		Capabilities:       []string{"state:kv", "iam:policy:evaluate"},
	}
	err := m.Validate()
	require.Error(t, err)
	require.True(t, IsErrManifestInvalid(err))
	require.Contains(t, err.Error(), "iam:policy:evaluate")

	// Requesting Tier 2 makes the manifest valid.
	m.TrustTierRequested = TierCore
	require.NoError(t, m.Validate())
}

// TestWASMRuntime_LoadModule_Valid verifies that a minimal valid WASM
// module with all mandatory entry points loads successfully.
func TestWASMRuntime_LoadModule_Valid(t *testing.T) {
	rt := NewWASMRuntime(NewCapabilityTable())
	defer rt.Close(testCtx(t))

	wasm := mustHexDecode(t, minimalWASMHex)
	cm, err := rt.LoadModule(testCtx(t), "test-plugin", "0.1.0", wasm)
	if err != nil {
		t.Fatalf("LoadModule failed: %v", err)
	}
	defer cm.Close(testCtx(t))
	if cm.Name() != "test-plugin" {
		t.Errorf("name = %q", cm.Name())
	}
}

// TestWASMRuntime_LoadModule_MissingEntryPoints verifies that a module
// missing wasm_init is rejected with ErrInvalidEntryPoints.
func TestWASMRuntime_LoadModule_MissingEntryPoints(t *testing.T) {
	rt := NewWASMRuntime(NewCapabilityTable())
	defer rt.Close(testCtx(t))

	wasm := mustHexDecode(t, minimalWASMMissingInitHex)
	_, err := rt.LoadModule(testCtx(t), "bad-plugin", "0.1.0", wasm)
	if err == nil {
		t.Fatal("expected ErrInvalidEntryPoints, got nil")
	}
	if !IsErrInvalidEntryPoints(err) {
		t.Fatalf("expected ErrInvalidEntryPoints, got %v", err)
	}
	t.Logf("got expected error: %v", err)
}

// TestWASMRuntime_Instantiate_CallEntry verifies that an instance can
// be created and its entry points invoked. wasm_init returns 0 (OK).
func TestWASMRuntime_Instantiate_CallEntry(t *testing.T) {
	rt := NewWASMRuntime(NewCapabilityTable())
	defer rt.Close(testCtx(t))

	wasm := mustHexDecode(t, minimalWASMHex)
	cm, err := rt.LoadModule(testCtx(t), "call-plugin", "0.1.0", wasm)
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	defer cm.Close(testCtx(t))

	inst, err := rt.Instantiate(testCtx(t), cm, "call-plugin", 0, []string{"state:kv"}, testCallTimeout())
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Destroy(testCtx(t))

	// wasm_init was already called by Instantiate; verify the token is
	// recognized by the capability table.
	if rt.CapabilityTable().Lookup(inst.Token) == nil {
		t.Error("instance token not registered in capability table")
	}

	// Call on_hook(token, hook_id=0, ctx_handle=0) — should return 0.
	status, err := inst.CallEntry(testCtx(t), "on_hook", 0, 0)
	if err != nil {
		t.Fatalf("CallEntry on_hook: %v", err)
	}
	if status != 0 {
		t.Errorf("on_hook returned %d, want 0", status)
	}

	// Call on_request(token, route_id=0, req_handle=0).
	status, err = inst.CallEntry(testCtx(t), "on_request", 0, 0)
	if err != nil {
		t.Fatalf("CallEntry on_request: %v", err)
	}
	if status != 0 {
		t.Errorf("on_request returned %d, want 0", status)
	}
}

// TestInstancePool_BorrowReturn covers the basic pool lifecycle.
func TestInstancePool_BorrowReturn(t *testing.T) {
	rt := NewWASMRuntime(NewCapabilityTable())
	defer rt.Close(testCtx(t))
	wasm := mustHexDecode(t, minimalWASMHex)
	cm, err := rt.LoadModule(testCtx(t), "pool-plugin", "0.1.0", wasm)
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	defer cm.Close(testCtx(t))

	pool := NewInstancePool(rt, cm, "pool-plugin", 0, []string{"state:kv"}, PoolConfig{
		MaxSize:       2,
		BorrowTimeout: 5 * time.Second, // long enough that waitFree blocks until Return
		CallTimeout:   testCallTimeout(),
	})
	defer pool.DestroyAll(testCtx(t))

	// Borrow two instances (should succeed, creating them on demand).
	i1, err := pool.Borrow(testCtx(t))
	if err != nil {
		t.Fatalf("borrow 1: %v", err)
	}
	i2, err := pool.Borrow(testCtx(t))
	if err != nil {
		t.Fatalf("borrow 2: %v", err)
	}
	if i1 == i2 {
		t.Error("pool returned same instance twice")
	}

	// Third borrow should block (pool at capacity). Spawn it and prove
	// it is still pending after a short delay — then return one instance
	// and the queued borrow should complete with success.
	done := make(chan error, 1)
	go func() {
		inst, err := pool.Borrow(testCtx(t))
		if err == nil {
			pool.Return(inst)
		}
		done <- err
	}()

	// Within 50ms the third borrow must still be blocked.
	select {
	case err := <-done:
		t.Fatalf("borrow should block at capacity, but returned err=%v", err)
	case <-time.After(50 * time.Millisecond):
		// OK — borrow is blocked as expected.
	}

	// Return one and the queued borrow should now succeed.
	pool.Return(i1)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected success after return, got %v", err)
		}
	case <-testCtx(t).Done():
		t.Fatal("re-borrow did not complete after return")
	}

	pool.Return(i2)
	stats := pool.Stats()
	if stats.Free != 2 || stats.InUse != 0 {
		t.Errorf("stats = %+v, want Free=2 InUse=0", stats)
	}
}

// TestInstancePool_Discard covers unhealthy instance handling — a
// discarded instance should not be returned to the free list.
func TestInstancePool_Discard(t *testing.T) {
	rt := NewWASMRuntime(NewCapabilityTable())
	defer rt.Close(testCtx(t))
	wasm := mustHexDecode(t, minimalWASMHex)
	cm, _ := rt.LoadModule(testCtx(t), "discard-plugin", "0.1.0", wasm)
	defer cm.Close(testCtx(t))

	pool := NewInstancePool(rt, cm, "discard-plugin", 0, nil, DefaultPoolConfig())
	defer pool.DestroyAll(testCtx(t))

	i, err := pool.Borrow(testCtx(t))
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	pool.Discard(i) // simulate a trap

	stats := pool.Stats()
	if stats.InUse != 0 {
		t.Errorf("InUse = %d after discard, want 0", stats.InUse)
	}
	if stats.Free != 0 {
		t.Errorf("Free = %d after discard, want 0 (discarded instance not returned)", stats.Free)
	}
}
