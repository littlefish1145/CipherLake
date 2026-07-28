package pluginloader

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero/api"
	"go.uber.org/zap"

	"cipherlake/internal/config"
	"cipherlake/internal/pipeline"
	pluginpb "cipherlake/proto/plugin"
)

// Small coverage tests for trivial helpers and error types that are easy to
// exercise in isolation. Grouped here so larger feature tests stay focused.

func TestCompiledModule_Version(t *testing.T) {
	cm := &CompiledModule{name: "p", version: "1.2.3"}
	require.Equal(t, "1.2.3", cm.Version())
}

func TestHTTPFetcher_SetLogger(t *testing.T) {
	f := NewHTTPFetcher()
	f.SetLogger(zap.NewNop())
	require.NotNil(t, f.logger)
}

func TestTrustStore_SHA256(t *testing.T) {
	ts := &TrustStore{}
	got := ts.SHA256([]byte("hello"))
	require.Equal(t, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", got)
}

func TestErrUnauthorized(t *testing.T) {
	err := ErrUnauthorized{Capability: "state:kv", Reason: "no"}
	require.Contains(t, err.Error(), "state:kv")
	require.True(t, IsErrUnauthorized(err))
	require.False(t, IsErrUnauthorized(errors.New("other")))
}

func TestCapabilityTable_Count(t *testing.T) {
	tbl := NewCapabilityTable()
	require.Equal(t, 0, tbl.Count())

	tok, err := tbl.Issue("p", 0, []string{"state:kv"}, neverExpires())
	require.NoError(t, err)
	require.Equal(t, 1, tbl.Count())

	tbl.Revoke(tok)
	require.Equal(t, 0, tbl.Count())
}

func TestLoader_RegisterHookInMemory(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	l.registerHookInMemory(&HookRecord{PluginName: "p", HookID: "h1", Operation: "s3:GetObject", HookType: "before"})
	require.Len(t, l.hooks.forOperation("s3:GetObject"), 1)
}

func TestWasmPipelinePlugin_Interface(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "pipeline-demo",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"pipeline:step:uppercase"},
		PipelineSteps:      []ManifestPipelineStep{{Name: "uppercase"}},
	}
	require.NoError(t, manifest.Validate())
	require.NoError(t, l.InstallPlugin(testCtx(t), manifest, minimalPipelinePluginBytes, nil, ""))

	plugin, ok := l.GetPipelineStep("uppercase")
	require.True(t, ok)
	require.Equal(t, "uppercase", plugin.Name())
	require.False(t, plugin.CanStream())
	require.Equal(t, []string{"*/*"}, plugin.SupportedTypes())

	result, err := plugin.Process(testCtx(t), &pipeline.ObjectInput{
		Key:     "docs/a.txt",
		Bucket:  "b",
		Content: strings.NewReader("hi"),
		Size:    2,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestLoader_InvokeHook_Normal(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "hooky",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"gateway:hook"},
		Hooks:              []ManifestHook{{Type: "before", Operation: "s3:GetObject", Handler: "on_hook"}},
	}
	require.NoError(t, manifest.Validate())
	require.NoError(t, l.InstallPlugin(testCtx(t), manifest, minimalPluginBytes, nil, ""))

	resp, err := l.InvokeHook(testCtx(t), &pluginpb.InvokeHookRequest{
		PluginName: "hooky",
		Handler:    "on_hook",
		Context:    &pluginpb.HookContext{Operation: "s3:GetObject", Phase: "before"},
	})
	require.NoError(t, err)
	require.Equal(t, pluginpb.HookAction_HOOK_ACTION_CONTINUE, resp.Action)
}

func TestLoader_InvokeHook_Reloading(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "hooky",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"state:kv"},
	}
	require.NoError(t, l.InstallPlugin(testCtx(t), manifest, minimalPluginBytes, nil, ""))

	l.mu.Lock()
	l.plugins["hooky"].reloading = true
	l.mu.Unlock()

	_, err := l.InvokeHook(testCtx(t), &pluginpb.InvokeHookRequest{
		PluginName: "hooky",
		Handler:    "on_hook",
		Context:    &pluginpb.HookContext{Operation: "s3:GetObject", Phase: "before"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reloading")
}

func TestLoader_CloseAllSSESessions(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "sse-demo",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"state:kv"},
	}
	require.NoError(t, l.InstallPlugin(testCtx(t), manifest, minimalPluginBytes, nil, ""))

	id := l.CreateSSESession("sse-demo")
	require.NotZero(t, id)

	l.CloseAllSSESessions(testCtx(t), "sse-demo", SSECloseReasonUninstall)
	require.Equal(t, "", l.GetSSESessionOwner(id))
}

func TestLoader_RestoreHooks(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "hooky",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"gateway:hook"},
		Hooks:              []ManifestHook{{Type: "before", Operation: "s3:GetObject", Handler: "on_hook"}},
	}
	require.NoError(t, manifest.Validate())
	require.NoError(t, l.InstallPlugin(testCtx(t), manifest, minimalPluginBytes, nil, ""))
	require.Len(t, l.hooks.all(), 1)

	l.hooks.clear("hooky")
	require.Len(t, l.hooks.all(), 0)

	require.NoError(t, l.restoreHooks())
	require.Len(t, l.hooks.all(), 1)
}

func TestErrorTypes(t *testing.T) {
	sessionErr := ErrSessionNotOwned{SessionID: 7, Owner: "a", PluginName: "b"}
	require.Contains(t, sessionErr.Error(), "session 7")
	require.True(t, IsErrSessionNotOwned(sessionErr))
	require.False(t, IsErrSessionNotOwned(errors.New("x")))

	depErr := ErrMissingDependency{Plugin: "p", Missing: "q"}
	require.Contains(t, depErr.Error(), "depends on")
	require.True(t, IsErrMissingDependency(depErr))
	require.False(t, IsErrMissingDependency(errors.New("x")))

	poolExhausted := ErrPoolExhausted{Plugin: "p", Max: 3}
	require.Contains(t, poolExhausted.Error(), "exhausted")

	poolClosed := ErrPoolClosed{Plugin: "p"}
	require.Contains(t, poolClosed.Error(), "closed")

	stepConflict := ErrPipelineStepConflict{Step: "s", ExistingPlugin: "p"}
	require.Contains(t, stepConflict.Error(), "pipeline step")

	sigErr := ErrInvalidSignature{Reason: "bad"}
	require.Contains(t, sigErr.Error(), "invalid plugin signature")
	require.True(t, IsErrInvalidSignature(sigErr))
	require.False(t, IsErrInvalidSignature(errors.New("x")))
}

func TestNewPluginLoader_ConfigErrors(t *testing.T) {
	_, err := New(nil)
	require.Error(t, err)

	_, err = New(&config.PluginLoaderConfig{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "raft config")
}

func TestNewPluginLoader_BadTrustStore(t *testing.T) {
	dir := t.TempDir()
	badKeys := filepath.Join(dir, "trusted_keys.json")
	require.NoError(t, os.WriteFile(badKeys, []byte("not json"), 0o644))

	cfg := &config.PluginLoaderConfig{
		CallTimeout:    "1s",
		PluginCacheDir: filepath.Join(dir, "cache"),
		TrustedKeysPath: badKeys,
		Raft: &config.RaftConfig{
			Enabled:    true,
			DataDir:    dir,
			NodeID:     "bad-trust-test",
			ListenAddr: "127.0.0.1:0",
		},
	}
	_, err := New(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "trust store")
}

func TestParseSlogLevel(t *testing.T) {
	require.Equal(t, slog.LevelDebug, parseSlogLevel("debug"))
	require.Equal(t, slog.LevelInfo, parseSlogLevel("info"))
	require.Equal(t, slog.LevelWarn, parseSlogLevel("warn"))
	require.Equal(t, slog.LevelError, parseSlogLevel("error"))
	require.Equal(t, slog.LevelInfo, parseSlogLevel("unknown"))
}

func TestNewHTTPFetcher(t *testing.T) {
	f := NewHTTPFetcher()
	require.NotNil(t, f.client)
	require.Equal(t, 30*time.Second, f.client.Timeout)
}

func TestHostImports_StateCAS_InvalidArgs(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("cas-plugin", 0, []string{"state:kv"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	h.stateCas(ctx, mod, uint64(tok), base, 4, base, 4, 0, base+256, base+260)
	require.Equal(t, statusInvalidArgs, readTestUint32(t, mem, base+256))
}

func TestLoader_OCIAuthForRegistry(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	l.cfg.OCRegistries = []config.OCIRegistryConfig{
		{URL: "https://reg.example.com", AuthType: "basic", Auth: config.OCIRegistryAuthConfig{UsernameEnv: "USER", PasswordEnv: "PASS"}},
		{URL: "http://token.example.com", AuthType: "bearer", Auth: config.OCIRegistryAuthConfig{TokenEnv: "TOKEN"}},
		{URL: "https://nomatch.example.com", AuthType: "none"},
	}

	t.Setenv("USER", "u")
	t.Setenv("PASS", "p")
	t.Setenv("TOKEN", "tok")

	auth := l.ociAuthForRegistry("reg.example.com")
	require.NotNil(t, auth)
	require.Equal(t, "basic", auth.Type)
	require.Equal(t, "u", auth.Username)

	auth = l.ociAuthForRegistry("token.example.com")
	require.NotNil(t, auth)
	require.Equal(t, "bearer", auth.Type)
	require.Equal(t, "tok", auth.Token)

	require.Nil(t, l.ociAuthForRegistry("unknown.example.com"))
}

// wasmInitFailsBytes is a minimal WASM module whose wasm_init returns 1.
var wasmInitFailsBytes = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // magic + version
	// type section: (i64) -> i32
	0x01, 0x06, 0x01, 0x60, 0x01, 0x7e, 0x01, 0x7f,
	// function section: 3 functions of type 0
	0x03, 0x04, 0x03, 0x00, 0x00, 0x00,
	// export section: 3 exports
	0x07, 0x24,
	0x03,
	0x09, 0x77, 0x61, 0x73, 0x6d, 0x5f, 0x69, 0x6e, 0x69, 0x74, 0x00, 0x00,
	0x07, 0x6f, 0x6e, 0x5f, 0x68, 0x6f, 0x6f, 0x6b, 0x00, 0x01,
	0x0a, 0x6f, 0x6e, 0x5f, 0x72, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x00, 0x02,
	// code section: 3 bodies
	0x0a, 0x10,
	0x03,
	// wasm_init: return 1
	0x04, 0x00, 0x41, 0x01, 0x0b,
	// on_hook: return 0
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// on_request: return 0
	0x04, 0x00, 0x41, 0x00, 0x0b,
}

func TestWASMRuntime_Instantiate_NonZeroWasmInit(t *testing.T) {
	rt := NewWASMRuntimeWithLimits(NewCapabilityTable(), NewResourceLimiter(config.PluginLoaderResourceLimits{}))
	defer rt.Close(testCtx(t))

	compiled, err := rt.LoadModule(testCtx(t), "fails", "1.0.0", wasmInitFailsBytes)
	require.NoError(t, err)

	_, err = rt.Instantiate(testCtx(t), compiled, "fails", 0, []string{"state:kv"}, 5*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wasm_init returned non-zero status")
}

// hookActionPluginBytes is a minimal WASM module with wasm_init/on_request
// returning 0 and on_hook returning a configurable status.
func makeHookActionPluginBytes(onHookStatus int32) []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		// type section: t0 (i64)->i32, t1 (i64,i64)->i32 (payload=12)
		0x01, 0x0c, 0x02,
		0x60, 0x01, 0x7e, 0x01, 0x7f,
		0x60, 0x02, 0x7e, 0x7e, 0x01, 0x7f,
		// function section: wasm_init=t0, on_hook=t1, on_request=t0
		0x03, 0x04, 0x03, 0x00, 0x01, 0x00,
		// export section
		0x07, 0x24, 0x03,
		0x09, 0x77, 0x61, 0x73, 0x6d, 0x5f, 0x69, 0x6e, 0x69, 0x74, 0x00, 0x00,
		0x07, 0x6f, 0x6e, 0x5f, 0x68, 0x6f, 0x6f, 0x6b, 0x00, 0x01,
		0x0a, 0x6f, 0x6e, 0x5f, 0x72, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x00, 0x02,
		// code section (payload=16)
		0x0a, 0x10, 0x03,
		// wasm_init: return 0
		0x04, 0x00, 0x41, 0x00, 0x0b,
		// on_hook: return status
		0x04, 0x00, 0x41, byte(onHookStatus), 0x0b,
		// on_request: return 0
		0x04, 0x00, 0x41, 0x00, 0x0b,
	}
}

func TestLoader_InvokeHook_Abort(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "aborty",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"gateway:hook"},
		Hooks:              []ManifestHook{{Type: "before", Operation: "s3:GetObject", Handler: "on_hook"}},
	}
	require.NoError(t, manifest.Validate())
	require.NoError(t, l.InstallPlugin(testCtx(t), manifest, makeHookActionPluginBytes(1), nil, ""))

	resp, err := l.InvokeHook(testCtx(t), &pluginpb.InvokeHookRequest{
		PluginName: "aborty",
		Handler:    "on_hook",
		Context:    &pluginpb.HookContext{Operation: "s3:GetObject", Phase: "before"},
	})
	require.NoError(t, err)
	require.Equal(t, pluginpb.HookAction_HOOK_ACTION_ABORT, resp.Action)
}

func TestLoader_InvokeHook_Modify(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "modify",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"gateway:hook"},
		Hooks:              []ManifestHook{{Type: "before", Operation: "s3:GetObject", Handler: "on_hook"}},
	}
	require.NoError(t, manifest.Validate())
	require.NoError(t, l.InstallPlugin(testCtx(t), manifest, makeHookActionPluginBytes(2), nil, ""))

	resp, err := l.InvokeHook(testCtx(t), &pluginpb.InvokeHookRequest{
		PluginName: "modify",
		Handler:    "on_hook",
		Context:    &pluginpb.HookContext{Operation: "s3:GetObject", Phase: "before", Bucket: "b"},
	})
	require.NoError(t, err)
	require.Equal(t, pluginpb.HookAction_HOOK_ACTION_MODIFY, resp.Action)
	require.NotNil(t, resp.ModifiedContext)
	require.Equal(t, "b", resp.ModifiedContext.Bucket)
}

func TestLoader_InvokeHook_InvalidStatus(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "bad-status",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"gateway:hook"},
		Hooks:              []ManifestHook{{Type: "before", Operation: "s3:GetObject", Handler: "on_hook"}},
	}
	require.NoError(t, manifest.Validate())
	require.NoError(t, l.InstallPlugin(testCtx(t), manifest, makeHookActionPluginBytes(99), nil, ""))

	_, err := l.InvokeHook(testCtx(t), &pluginpb.InvokeHookRequest{
		PluginName: "bad-status",
		Handler:    "on_hook",
		Context:    &pluginpb.HookContext{Operation: "s3:GetObject", Phase: "before"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid status")
}

func TestLoader_InvokeHook_InputValidation(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	_, err := l.InvokeHook(testCtx(t), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "request is nil")

	_, err = l.InvokeHook(testCtx(t), &pluginpb.InvokeHookRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "plugin_name is required")

	_, err = l.InvokeHook(testCtx(t), &pluginpb.InvokeHookRequest{PluginName: "p"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "handler is required")
}

func TestLoader_InvokeHook_PluginNotInstalled(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	_, err := l.InvokeHook(testCtx(t), &pluginpb.InvokeHookRequest{
		PluginName: "missing",
		Handler:    "on_hook",
		Context:    &pluginpb.HookContext{Operation: "s3:GetObject", Phase: "before"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not installed")
}

func TestCapabilityTable_IssueValidation(t *testing.T) {
	tbl := NewCapabilityTable()
	_, err := tbl.Issue("", 0, []string{"state:kv"}, neverExpires())
	require.Error(t, err)
	require.Contains(t, err.Error(), "plugin name is required")
}

func TestCapabilityTable_LookupExpired(t *testing.T) {
	tbl := NewCapabilityTable()
	tok, err := tbl.Issue("expired", 0, []string{"state:kv"}, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Nil(t, tbl.Lookup(tok))
}

// hostImportTestEnv sets up a HostImports layer backed by a test loader and a
// memory module that tests can use to call host-import functions directly.
type hostImportTestEnv struct {
	loader *Loader
	h      *HostImports
	mod    api.Module
	mem    api.Memory
	ctx    context.Context
}

func newHostImportTestEnv(t *testing.T, caps []string) *hostImportTestEnv {
	t.Helper()
	l := newTestLoader(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	ctx := testCtx(t)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	t.Cleanup(func() { mod.Close(ctx); l.Shutdown() })

	if len(caps) > 0 {
		tok, err := l.WASMRuntime().CapabilityTable().Issue("env-plugin", 0, caps, neverExpires())
		require.NoError(t, err)
		h.OnModuleInstantiated(mod, tok)
	}

	return &hostImportTestEnv{
		loader: l,
		h:      h,
		mod:    mod,
		mem:    mod.Memory(),
		ctx:    ctx,
	}
}

func TestHostImports_StateGetErrors(t *testing.T) {
	e := newHostImportTestEnv(t, []string{"state:kv"})
	const base uint32 = 64

	// Missing capability token -> unauthorized.
	e.h.stateGet(e.ctx, e.mod, 0, base, 4, base+128, 128, base+256, base+260, base+264)
	require.Equal(t, statusUnauthorized, readTestUint32(t, e.mem, base+256))

	// Invalid key pointer -> invalid args.
	tok := e.h.caps.Lookup(e.h.tokenForModule(e.mod))
	require.NotNil(t, tok)
	e.h.stateGet(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), 0xFFFFFFFF, 4, base+128, 128, base+256, base+260, base+264)
	require.Equal(t, statusInvalidArgs, readTestUint32(t, e.mem, base+256))

	// Key not found.
	writeTestMemory(t, e.mem, base, []byte("missing"))
	e.h.stateGet(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), base, 7, base+128, 128, base+256, base+260, base+264)
	require.Equal(t, statusNotFound, readTestUint32(t, e.mem, base+256))

	// Buffer too small.
	writeTestMemory(t, e.mem, base, []byte("tiny"))
	_, err := e.loader.StatePut("env-plugin", "tiny", []byte("value"), 0)
	require.NoError(t, err)
	e.h.stateGet(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), base, 4, base+128, 2, base+256, base+260, base+264)
	require.Equal(t, statusBufferTooSmall, readTestUint32(t, e.mem, base+256))
}

func TestHostImports_StateBatchErrors(t *testing.T) {
	e := newHostImportTestEnv(t, []string{"state:kv"})
	const base uint32 = 64

	// Missing capability.
	e.h.stateBatch(e.ctx, e.mod, 0, base, 4, base+256, base+512, 128, base+260)
	require.Equal(t, statusUnauthorized, readTestUint32(t, e.mem, base+256))

	// Invalid ops JSON.
	writeTestMemory(t, e.mem, base, []byte("not-json"))
	e.h.stateBatch(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), base, 8, base+256, base+512, 128, base+260)
	require.Equal(t, statusInvalidArgs, readTestUint32(t, e.mem, base+256))
}

func TestHostImports_HookReadErrors(t *testing.T) {
	const base uint32 = 64

	// No capability: module token lacks gateway:hook.
	eNoCap := newHostImportTestEnv(t, []string{"state:kv"})
	eNoCap.h.hookRead(eNoCap.ctx, eNoCap.mod, 1, base, 1024, base+256, base+260)
	require.Equal(t, statusUnauthorized, readTestUint32(t, eNoCap.mem, base+260))

	// Invalid handle and buffer too small.
	e := newHostImportTestEnv(t, []string{"gateway:hook"})

	// Invalid handle.
	e.h.hookRead(e.ctx, e.mod, 0, base, 1024, base+256, base+260)
	require.Equal(t, statusInvalidArgs, readTestUint32(t, e.mem, base+260))

	// Buffer too small.
	inv := &hookInvocation{pluginName: "env-plugin", ctx: &pluginpb.HookContext{Operation: "s3:GetObject"}}
	handle := e.loader.allocateHookContext(inv)
	e.h.hookRead(e.ctx, e.mod, handle, base, 0, base+256, base+260)
	require.Equal(t, statusBufferTooSmall, readTestUint32(t, e.mem, base+260))
}

func TestHostImports_HookBodyErrors(t *testing.T) {
	e := newHostImportTestEnv(t, []string{"gateway:hook"})
	const base uint32 = 64

	inv := &hookInvocation{pluginName: "env-plugin", ctx: &pluginpb.HookContext{Body: []byte("hello")}}
	handle := e.loader.allocateHookContext(inv)

	// Buffer too small.
	e.h.hookBody(e.ctx, e.mod, handle, base, 0, base+256, base+260)
	require.Equal(t, statusBufferTooSmall, readTestUint32(t, e.mem, base+260))
}

func TestHostImports_RequestReadErrors(t *testing.T) {
	e := newHostImportTestEnv(t, nil)
	const base uint32 = 64

	// Invalid handle.
	e.h.requestRead(e.ctx, e.mod, 0, base, 1024, base+256, base+260)
	require.Equal(t, statusInvalidArgs, readTestUint32(t, e.mem, base+260))

	// Buffer too small.
	req := &pluginRequest{method: "GET", path: "/x", body: []byte("body")}
	handle := e.loader.allocateRequest(req)
	e.h.requestRead(e.ctx, e.mod, handle, base, 0, base+256, base+260)
	require.Equal(t, statusBufferTooSmall, readTestUint32(t, e.mem, base+260))
}

func TestHostImports_PipelineReadErrors(t *testing.T) {
	e := newHostImportTestEnv(t, []string{"pipeline:step"})
	const base uint32 = 64

	// No capability.
	e.h.pipelineRead(e.ctx, e.mod, 0, 0, base, 1024, base+256, base+260)
	require.Equal(t, statusUnauthorized, readTestUint32(t, e.mem, base+260))

	// Invalid handle.
	e.h.pipelineRead(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), 1, base, 1024, base+256, base+260)
	require.Equal(t, statusNotFound, readTestUint32(t, e.mem, base+260))

	// Buffer too small.
	inv := &pipelineInvocation{pluginName: "env-plugin", stepName: "x", userMetadata: map[string]string{"k": "v"}}
	handle := e.loader.allocatePipelineContext(inv)
	e.h.pipelineRead(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), handle, base, 0, base+256, base+260)
	require.Equal(t, statusBufferTooSmall, readTestUint32(t, e.mem, base+260))
}

func TestHostImports_EventReadErrors(t *testing.T) {
	e := newHostImportTestEnv(t, []string{"event:subscribe"})
	const base uint32 = 64

	// No capability.
	e.h.eventRead(e.ctx, e.mod, 0, 0, base, 1024, base+256, base+260)
	require.Equal(t, statusUnauthorized, readTestUint32(t, e.mem, base+260))

	// Invalid handle.
	e.h.eventRead(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), 1, base, 1024, base+256, base+260)
	require.Equal(t, statusNotFound, readTestUint32(t, e.mem, base+260))

	// Buffer too small.
	inv := &eventInvocation{payload: []byte("hello")}
	handle := e.loader.allocateEventContext(inv)
	e.h.eventRead(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), handle, base, 0, base+256, base+260)
	require.Equal(t, statusBufferTooSmall, readTestUint32(t, e.mem, base+260))
}

func TestHostImports_TaskReadErrors(t *testing.T) {
	e := newHostImportTestEnv(t, []string{"task:schedule"})
	const base uint32 = 64

	// No capability.
	e.h.taskRead(e.ctx, e.mod, 0, 0, base, 1024, base+256, base+260)
	require.Equal(t, statusUnauthorized, readTestUint32(t, e.mem, base+260))

	// Invalid handle.
	e.h.taskRead(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), 1, base, 1024, base+256, base+260)
	require.Equal(t, statusNotFound, readTestUint32(t, e.mem, base+260))

	// Buffer too small.
	inv := &taskInvocation{taskType: "t", payload: []byte("hello")}
	handle := e.loader.allocateTaskContext(inv)
	e.h.taskRead(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)), handle, base, 0, base+256, base+260)
	require.Equal(t, statusBufferTooSmall, readTestUint32(t, e.mem, base+260))
}

