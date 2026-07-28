package pluginloader

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"cipherlake/internal/config"
	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
)

func newTestAdminLoader(t *testing.T) *Loader {
	t.Helper()
	cfg := &config.PluginLoaderConfig{
		CallTimeout:    "1s",
		PluginCacheDir: filepath.Join(t.TempDir(), "plugin-cache"),
		Raft: &config.RaftConfig{
			Enabled:    true,
			DataDir:    t.TempDir(),
			NodeID:     "admin-test",
			ListenAddr: "127.0.0.1:0",
		},
	}
	loader, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = loader.Shutdown() })

	// Wait for leader election.
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := loader.RaftApply(ctx, &LoaderFSMOp{Type: OpStatePut, Plugin: "_probe", Key: "k", Data: []byte("v")}); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return loader
}

func minimalManifestBytes(name string) []byte {
	return []byte(`{
		"manifest_version": "1.0",
		"api_version": ">=1.0.0 <2.0.0",
		"name": "` + name + `",
		"version": "1.0.0",
		"trust_tier_requested": 0,
		"capabilities": ["state:kv"]
	}`)
}

func TestLoaderAdminGRPCServer_ListPlugins_Empty(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}

	resp, err := admin.ListPlugins(context.Background(), &pluginpb.ListPluginsRequest{})
	require.NoError(t, err)
	require.Empty(t, resp.Plugins)
}

func TestLoaderAdminGRPCServer_GetPluginState_NotFound(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}

	resp, err := admin.GetPluginState(context.Background(), &pluginpb.GetPluginStateRequest{
		PluginName: "missing",
		Key:        "k",
	})
	require.NoError(t, err)
	require.False(t, resp.Found)
}

func TestLoaderAdminGRPCServer_DependentsBlockUninstall(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}

	// Simulate two installed plugins via direct FSM writes.
	ctx := context.Background()
	baseRecord := PluginRecord{
		Name:        "base",
		Version:     "1.0.0",
		TrustTier:   0,
		Manifest:    minimalManifestBytes("base"),
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	dependentRecord := PluginRecord{
		Name:    "dependent",
		Version: "1.0.0",
		TrustTier: 0,
		Manifest: []byte(`{
			"manifest_version": "1.0",
			"api_version": ">=1.0.0 <2.0.0",
			"name": "dependent",
			"version": "1.0.0",
			"trust_tier_requested": 0,
			"depends_on": ["base"]
		}`),
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}

	baseJSON, _ := json.Marshal(baseRecord)
	depJSON, _ := json.Marshal(dependentRecord)
	require.NoError(t, loader.RaftApply(ctx, &LoaderFSMOp{Type: OpPluginInstall, Plugin: "base", Data: baseJSON}))
	require.NoError(t, loader.RaftApply(ctx, &LoaderFSMOp{Type: OpPluginInstall, Plugin: "dependent", Data: depJSON}))

	_, err := admin.UninstallPlugin(ctx, &pluginpb.UninstallPluginRequest{PluginName: "base"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dependents")

	// Force uninstall succeeds.
	resp, err := admin.UninstallPlugin(ctx, &pluginpb.UninstallPluginRequest{PluginName: "base", Force: true})
	require.NoError(t, err)
	require.True(t, resp.Uninstalled)
}

func TestLoaderAdminGRPCServer_ListPlugins_AfterFSMWrite(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}

	record := PluginRecord{
		Name:        "demo",
		Version:     "1.2.3",
		TrustTier:   1,
		Manifest:    minimalManifestBytes("demo"),
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	recordJSON, _ := json.Marshal(record)
	require.NoError(t, loader.RaftApply(context.Background(), &LoaderFSMOp{Type: OpPluginInstall, Plugin: "demo", Data: recordJSON}))

	resp, err := admin.ListPlugins(context.Background(), &pluginpb.ListPluginsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Plugins, 1)
	require.Equal(t, "demo", resp.Plugins[0].Name)
	require.Equal(t, "1.2.3", resp.Plugins[0].Version)
	require.Equal(t, int32(1), resp.Plugins[0].TrustTier)
}

func TestPluginCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	cache := newPluginCache(dir, "demo")

	require.NoError(t, cache.WriteManifest([]byte("manifest")))
	require.NoError(t, cache.WriteWASM([]byte("wasm")))
	require.NoError(t, cache.WriteSignature([]byte("sig")))
	require.NoError(t, cache.WriteKeyID("key-1"))

	m, err := cache.ReadManifest()
	require.NoError(t, err)
	require.Equal(t, []byte("manifest"), m)

	w, err := cache.ReadWASM()
	require.NoError(t, err)
	require.Equal(t, []byte("wasm"), w)

	s, err := cache.ReadSignature()
	require.NoError(t, err)
	require.Equal(t, []byte("sig"), s)

	kid, err := cache.ReadKeyID()
	require.NoError(t, err)
	require.Equal(t, "key-1", kid)
}

func TestLoader_WriteAndReadPluginCache(t *testing.T) {
	loader := newTestAdminLoader(t)
	record := &PluginRecord{
		Name:         "demo",
		SignatureB64: base64.StdEncoding.EncodeToString([]byte("sig")),
		KeyID:        "key-1",
	}
	require.NoError(t, loader.writePluginCache(record, []byte("manifest"), []byte("wasm")))

	cacheDir := filepath.Join(loader.cacheDir(), "demo")
	require.FileExists(t, filepath.Join(cacheDir, "manifest.json"))
	require.FileExists(t, filepath.Join(cacheDir, "plugin.wasm"))
	require.FileExists(t, filepath.Join(cacheDir, "signature.sig"))
	require.FileExists(t, filepath.Join(cacheDir, "key_id.txt"))

	m, w, s, k, err := loader.readPluginCache("demo")
	require.NoError(t, err)
	require.Equal(t, []byte("manifest"), m)
	require.Equal(t, []byte("wasm"), w)
	require.Equal(t, []byte("sig"), s)
	require.Equal(t, "key-1", k)
}

func TestPluginCache_EmptyDir(t *testing.T) {
	cache := &pluginCache{dir: ""}
	require.Error(t, cache.ensureDir())
	require.Error(t, cache.WriteManifest([]byte("m")))
}

func TestLoader_StateDelete(t *testing.T) {
	loader := newTestAdminLoader(t)

	_, err := loader.StatePut("demo", "k", []byte("v"), 0)
	require.NoError(t, err)
	entry, err := loader.StateGet("demo", "k")
	require.NoError(t, err)
	require.NotNil(t, entry)

	require.NoError(t, loader.StateDelete("demo", "k"))
	entry, err = loader.StateGet("demo", "k")
	require.NoError(t, err)
	require.Nil(t, entry)
}

func TestLoader_RegisterRouteAndRoutePlugin(t *testing.T) {
	loader := newTestAdminLoader(t)
	require.NoError(t, loader.RegisterRoute("/demo/*", "demo"))
	require.Equal(t, "demo", loader.RoutePlugin("/demo/*"))
	require.Equal(t, "", loader.RoutePlugin("/other"))
}

func TestLoader_Health(t *testing.T) {
	loader := newTestAdminLoader(t)
	resp, err := loader.Health(context.Background())
	require.NoError(t, err)
	require.True(t, resp.Healthy)
}

func manifestBytesWithRoutes(name, prefix string) []byte {
	return []byte(`{
		"manifest_version": "1.0",
		"api_version": ">=1.0.0 <2.0.0",
		"name": "` + name + `",
		"version": "1.0.0",
		"trust_tier_requested": 0,
		"capabilities": ["http:route", "state:kv"],
		"routes": [{"prefix": "` + prefix + `", "handler": "on_request"}]
	}`)
}

func TestLoaderAdminGRPCServer_InstallPlugin_Success(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}
	ctx := context.Background()

	manifest := manifestBytesWithRoutes("install-success", "/install/*")
	wasm := minimalPluginBytes

	resp, err := admin.InstallPlugin(ctx, &pluginpb.InstallPluginRequest{
		ManifestBytes: manifest,
		WasmBytes:     wasm,
	})
	require.NoError(t, err)
	require.Equal(t, "install-success", resp.PluginName)
	require.Equal(t, "1.0.0", resp.Version)

	// Artifacts should be persisted to the local cache.
	require.FileExists(t, filepath.Join(loader.cacheDir(), "install-success", "manifest.json"))
	require.FileExists(t, filepath.Join(loader.cacheDir(), "install-success", "plugin.wasm"))
}

func TestLoaderAdminGRPCServer_InstallPlugin_ValidationErrors(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}
	ctx := context.Background()

	_, err := admin.InstallPlugin(ctx, &pluginpb.InstallPluginRequest{
		ManifestBytes: nil,
		WasmBytes:     minimalPluginBytes,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "manifest_bytes")

	_, err = admin.InstallPlugin(ctx, &pluginpb.InstallPluginRequest{
		ManifestBytes: minimalManifestBytes("missing-wasm"),
		WasmBytes:     nil,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "wasm_bytes")

	_, err = admin.InstallPlugin(ctx, &pluginpb.InstallPluginRequest{
		ManifestBytes: []byte(`{not json`),
		WasmBytes:     minimalPluginBytes,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid manifest")
}

func TestLoaderAdminGRPCServer_InstallPlugin_RouteConflict(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}
	ctx := context.Background()

	_, err := admin.InstallPlugin(ctx, &pluginpb.InstallPluginRequest{
		ManifestBytes: manifestBytesWithRoutes("first", "/conflict/*"),
		WasmBytes:     minimalPluginBytes,
	})
	require.NoError(t, err)

	_, err = admin.InstallPlugin(ctx, &pluginpb.InstallPluginRequest{
		ManifestBytes: manifestBytesWithRoutes("second", "/conflict/*"),
		WasmBytes:     minimalPluginBytes,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "AlreadyExists")
}

func TestLoader_GetPluginRecord(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}
	ctx := context.Background()

	// Missing plugin returns nil without error.
	record, err := loader.GetPluginRecord("missing")
	require.NoError(t, err)
	require.Nil(t, record)

	_, err = admin.InstallPlugin(ctx, &pluginpb.InstallPluginRequest{
		ManifestBytes: minimalManifestBytes("record-demo"),
		WasmBytes:     minimalPluginBytes,
	})
	require.NoError(t, err)

	record, err = loader.GetPluginRecord("record-demo")
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, "record-demo", record.Name)
	require.Equal(t, "1.0.0", record.Version)
}

func TestLoaderAdminGRPCServer_ReloadPlugin_Success(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}
	ctx := context.Background()

	_, err := admin.InstallPlugin(ctx, &pluginpb.InstallPluginRequest{
		ManifestBytes: minimalManifestBytes("reload-demo"),
		WasmBytes:     minimalPluginBytes,
	})
	require.NoError(t, err)

	resp, err := admin.ReloadPlugin(ctx, &pluginpb.ReloadPluginRequest{PluginName: "reload-demo"})
	require.NoError(t, err)
	require.Equal(t, "reload-demo", resp.PluginName)
	require.Equal(t, "1.0.0", resp.Version)
}

func TestLoaderAdminGRPCServer_ReloadPlugin_NotFound(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}
	ctx := context.Background()

	_, err := admin.ReloadPlugin(ctx, &pluginpb.ReloadPluginRequest{PluginName: "missing"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestLoaderAdminGRPCServer_TrustedKeys(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}
	ctx := context.Background()

	// Empty list initially.
	listResp, err := admin.ListTrustedKeys(ctx, &pluginpb.ListTrustedKeysRequest{})
	require.NoError(t, err)
	require.Empty(t, listResp.Keys)

	// Add a key.
	pub, _, _ := ed25519.GenerateKey(nil)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	addResp, err := admin.AddTrustedKey(ctx, &pluginpb.AddTrustedKeyRequest{
		KeyId:     "admin-key",
		PublicKey: pubB64,
		IsCore:    true,
		AddedBy:   "test-admin",
	})
	require.NoError(t, err)
	require.True(t, addResp.Added)

	// List contains the key.
	listResp, err = admin.ListTrustedKeys(ctx, &pluginpb.ListTrustedKeysRequest{})
	require.NoError(t, err)
	require.Len(t, listResp.Keys, 1)
	require.Equal(t, "admin-key", listResp.Keys[0].KeyId)
	require.True(t, listResp.Keys[0].IsCore)

	// Remove the key.
	removeResp, err := admin.RemoveTrustedKey(ctx, &pluginpb.RemoveTrustedKeyRequest{KeyId: "admin-key"})
	require.NoError(t, err)
	require.True(t, removeResp.Removed)

	listResp, err = admin.ListTrustedKeys(ctx, &pluginpb.ListTrustedKeysRequest{})
	require.NoError(t, err)
	require.Empty(t, listResp.Keys)
}

func TestLoaderAdminGRPCServer_TrustedKeys_ValidationErrors(t *testing.T) {
	loader := newTestAdminLoader(t)
	admin := &LoaderAdminGRPCServer{Loader: loader}
	ctx := context.Background()

	_, err := admin.AddTrustedKey(ctx, &pluginpb.AddTrustedKeyRequest{
		KeyId:     "",
		PublicKey: "abc",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "key_id")

	_, err = admin.AddTrustedKey(ctx, &pluginpb.AddTrustedKeyRequest{
		KeyId:     "k",
		PublicKey: "",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "public_key")

	_, err = admin.RemoveTrustedKey(ctx, &pluginpb.RemoveTrustedKeyRequest{KeyId: ""})
	require.Error(t, err)
	require.Contains(t, err.Error(), "key_id")
}

func TestLoader_SetVectorManager(t *testing.T) {
	loader := newTestAdminLoader(t)
	mock := &mockVectorSearcher{}
	loader.SetVectorManager(mock)

	host := loader.WASMRuntime().hostInst.(*HostImports)
	require.Equal(t, mock, host.vector)
}

func TestLoaderGRPCServer_Health(t *testing.T) {
	loader := newTestAdminLoader(t)
	srv := &LoaderGRPCServer{Loader: loader}
	resp, err := srv.Health(context.Background(), &pluginpb.HealthRequest{})
	require.NoError(t, err)
	require.True(t, resp.Healthy)
}

func TestLoaderGRPCServer_ListHooks_Empty(t *testing.T) {
	loader := newTestAdminLoader(t)
	srv := &LoaderGRPCServer{Loader: loader}
	resp, err := srv.ListHooks(context.Background(), &pluginpb.ListHooksRequest{})
	require.NoError(t, err)
	require.Empty(t, resp.Hooks)
}

func TestLoaderGRPCServer_InvokeRoute_PluginNotFound(t *testing.T) {
	loader := newTestAdminLoader(t)
	srv := &LoaderGRPCServer{Loader: loader}
	_, err := srv.InvokeRoute(context.Background(), &pluginpb.InvokeRouteRequest{
		PluginName: "missing",
		Method:     "GET",
		Path:       "/x",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not installed")
}

func TestLoaderGRPCServer_InvokeHook_NoPlugin(t *testing.T) {
	loader := newTestAdminLoader(t)
	srv := &LoaderGRPCServer{Loader: loader}
	_, err := srv.InvokeHook(context.Background(), &pluginpb.InvokeHookRequest{
		PluginName: "missing",
		Handler:    "on_hook",
		Context: &pluginpb.HookContext{
			Operation: "s3:GetObject",
			Phase:     "before",
		},
	})
	require.Error(t, err)
}

func TestLoader_RestoreTrustedKeyFromFSM(t *testing.T) {
	loader := newTestAdminLoader(t)
	ctx := context.Background()

	pub, _, _ := ed25519.GenerateKey(nil)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	require.NoError(t, loader.AddTrustedKey(ctx, "restore-key", pubB64, false, "test"))
	require.Len(t, loader.ListTrustedKeys(), 1)

	// Simulate in-memory loss and restore from the FSM mirror.
	loader.trust.RemoveKey("restore-key")
	require.Empty(t, loader.ListTrustedKeys())

	require.NoError(t, loader.restoreTrustedKeyFromFSM("restore-key"))
	keys := loader.ListTrustedKeys()
	require.Len(t, keys, 1)
	require.Equal(t, "restore-key", keys[0].KeyID)
}
