package pluginloader

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"cipherlake/internal/config"
	"cipherlake/internal/events"
	"cipherlake/internal/vector"
	pluginpb "cipherlake/proto/plugin"
)

// newTestLoader creates a Loader backed by a temporary Raft data directory.
// The caller must call Shutdown on the returned loader.
func newTestLoader(t testing.TB) *Loader {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.PluginLoaderConfig{
		Raft: &config.RaftConfig{
			Enabled:    true,
			DataDir:    dir,
			NodeID:     "loader-test",
			ListenAddr: "127.0.0.1:0",
		},
	}
	l, err := New(cfg)
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}
	// Wait for the single-node cluster to elect itself leader so that
	// RaftApply calls in tests do not fail with "not leader".
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l.raft.IsLeader() {
			return l
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("loader raft node did not become leader within 5s")
	return nil
}

// newTestMemoryModule creates a tiny guest module that owns a 64KB memory
// exported as "memory". Tests pass this module to host-import functions so
// they have a linear memory to read/write buffers.
func newTestMemoryModule(t testing.TB, ctx context.Context, rt wazero.Runtime) api.Module {
	t.Helper()
	cm, err := rt.CompileModule(ctx, memoryModuleBytes)
	if err != nil {
		t.Fatalf("compile memory module: %v", err)
	}
	mod, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName("mem"))
	if err != nil {
		t.Fatalf("instantiate memory module: %v", err)
	}
	return mod
}

// memoryModuleBytes is a hand-built WASM module:
//
//	(module (memory 1) (export "memory" (memory 0)))
var memoryModuleBytes = []byte{
	0x00, 0x61, 0x73, 0x6d, // magic
	0x01, 0x00, 0x00, 0x00, // version
	0x05, 0x03, 0x01, 0x00, 0x01, // memory section: 1 memory, 0, 1 page
	0x07, 0x0a, 0x01, // export section (size=10, count=1)
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, // "memory"
	0x02, 0x00, // memory export, index 0
}

func writeTestMemory(t testing.TB, mem api.Memory, ptr uint32, data []byte) {
	t.Helper()
	if !mem.Write(ptr, data) {
		t.Fatalf("failed to write test memory at %d", ptr)
	}
}

func readTestUint32(t testing.TB, mem api.Memory, ptr uint32) uint32 {
	t.Helper()
	b, ok := mem.Read(ptr, 4)
	if !ok {
		t.Fatalf("failed to read u32 at %d", ptr)
	}
	return binary.LittleEndian.Uint32(b)
}

func readTestUint64(t testing.TB, mem api.Memory, ptr uint32) uint64 {
	t.Helper()
	b, ok := mem.Read(ptr, 8)
	if !ok {
		t.Fatalf("failed to read u64 at %d", ptr)
	}
	return binary.LittleEndian.Uint64(b)
}

func TestHostImports_StatePutGet(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("test-plugin", 0, []string{"state:kv"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	key := []byte("session/abc")
	value := []byte(`{"count":1}`)
	writeTestMemory(t, mem, base, key)
	writeTestMemory(t, mem, base+128, value)

	// state.put(token, key_ptr, key_len, val_ptr, val_len, expected_version, out_status, out_version)
	h.statePut(ctx, mod, uint64(tok),
		base, uint32(len(key)),
		base+128, uint32(len(value)),
		0,
		base+256,
		base+260,
	)
	if readTestUint32(t, mem, base+256) != statusOK {
		t.Fatalf("state.put status = %d", readTestUint32(t, mem, base+256))
	}
	if readTestUint64(t, mem, base+260) != 1 {
		t.Fatalf("state.put version = %d", readTestUint64(t, mem, base+260))
	}

	// state.get(token, key_ptr, key_len, val_ptr, val_cap, out_status, out_len, out_version)
	h.stateGet(ctx, mod, uint64(tok),
		base, uint32(len(key)),
		base+512, 128,
		base+256,
		base+260,
		base+268,
	)
	if readTestUint32(t, mem, base+256) != statusOK {
		t.Fatalf("state.get status = %d", readTestUint32(t, mem, base+256))
	}
	if readTestUint32(t, mem, base+260) != uint32(len(value)) {
		t.Fatalf("state.get len = %d", readTestUint32(t, mem, base+260))
	}
	if readTestUint64(t, mem, base+268) != 1 {
		t.Fatalf("state.get version = %d", readTestUint64(t, mem, base+268))
	}
	got, _ := mem.Read(base+512, uint32(len(value)))
	if string(got) != string(value) {
		t.Fatalf("state.get value = %q", got)
	}
}

func manifestBytesWithGrants(name string, grants []ManifestGrant) []byte {
	m := Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               name,
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"state:kv"},
		Grants:             grants,
	}
	b, _ := json.Marshal(m)
	return b
}

func TestHostImports_CrossPluginStateGet(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Install plugin-a with a grant allowing plugin-b to read shared/* keys.
	aManifest := manifestBytesWithGrants("plugin-a", []ManifestGrant{
		{Plugin: "plugin-b", Keys: []string{"shared/*"}},
	})
	require.NoError(t, l.InstallPlugin(ctx, mustParseManifest(aManifest), minimalPluginBytes, nil, ""))

	// Install plugin-b (reader) without grants.
	bManifest := manifestBytesWithGrants("plugin-b", nil)
	require.NoError(t, l.InstallPlugin(ctx, mustParseManifest(bManifest), minimalPluginBytes, nil, ""))

	// plugin-a writes "shared/foo".
	_, err := l.StatePut("plugin-a", "shared/foo", []byte("hello-a"), 0)
	require.NoError(t, err)

	// plugin-b reads plugin-a:shared/foo with the cross_plugin capability.
	tok, err := l.WASMRuntime().CapabilityTable().Issue("plugin-b", 0, []string{"state:kv", "state:cross_plugin:plugin-a"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	key := []byte("plugin-a:shared/foo")
	writeTestMemory(t, mem, base, key)
	h.stateGet(ctx, mod, uint64(tok),
		base, uint32(len(key)),
		base+512, 128,
		base+256,
		base+260,
		base+268,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+256))
	require.Equal(t, uint32(len("hello-a")), readTestUint32(t, mem, base+260))
	got, _ := mem.Read(base+512, uint32(len("hello-a")))
	require.Equal(t, "hello-a", string(got))
}

func TestHostImports_CrossPluginStateGet_DeniedWithoutCapability(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	aManifest := manifestBytesWithGrants("plugin-a", []ManifestGrant{
		{Plugin: "plugin-b", Keys: []string{"shared/*"}},
	})
	require.NoError(t, l.InstallPlugin(ctx, mustParseManifest(aManifest), minimalPluginBytes, nil, ""))
	bManifest := manifestBytesWithGrants("plugin-b", nil)
	require.NoError(t, l.InstallPlugin(ctx, mustParseManifest(bManifest), minimalPluginBytes, nil, ""))
	_, err := l.StatePut("plugin-a", "shared/foo", []byte("hello-a"), 0)
	require.NoError(t, err)

	// plugin-b only has state:kv, not state:cross_plugin:plugin-a.
	tok, err := l.WASMRuntime().CapabilityTable().Issue("plugin-b", 0, []string{"state:kv"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	key := []byte("plugin-a:shared/foo")
	writeTestMemory(t, mem, base, key)
	h.stateGet(ctx, mod, uint64(tok),
		base, uint32(len(key)),
		base+512, 128,
		base+256,
		base+260,
		base+268,
	)
	require.Equal(t, statusUnauthorized, readTestUint32(t, mem, base+256))
}

func TestHostImports_CrossPluginStateGet_DeniedWithoutGrant(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// plugin-a does NOT grant plugin-b.
	aManifest := manifestBytesWithGrants("plugin-a", nil)
	require.NoError(t, l.InstallPlugin(ctx, mustParseManifest(aManifest), minimalPluginBytes, nil, ""))
	bManifest := manifestBytesWithGrants("plugin-b", nil)
	require.NoError(t, l.InstallPlugin(ctx, mustParseManifest(bManifest), minimalPluginBytes, nil, ""))
	_, err := l.StatePut("plugin-a", "shared/foo", []byte("hello-a"), 0)
	require.NoError(t, err)

	tok, err := l.WASMRuntime().CapabilityTable().Issue("plugin-b", 0, []string{"state:kv", "state:cross_plugin:plugin-a"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	key := []byte("plugin-a:shared/foo")
	writeTestMemory(t, mem, base, key)
	h.stateGet(ctx, mod, uint64(tok),
		base, uint32(len(key)),
		base+512, 128,
		base+256,
		base+260,
		base+268,
	)
	require.Equal(t, statusUnauthorized, readTestUint32(t, mem, base+256))
}

func mustParseManifest(b []byte) *Manifest {
	m, err := ParseManifest(b)
	if err != nil {
		panic(err)
	}
	return m
}

func TestHostImports_StateCAS(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("cas-plugin", 0, []string{"state:kv"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	key := []byte("counter")
	writeTestMemory(t, mem, base, key)

	put := func(v []byte, expected uint64) uint64 {
		writeTestMemory(t, mem, base+128, v)
		h.statePut(ctx, mod, uint64(tok),
			base, uint32(len(key)),
			base+128, uint32(len(v)),
			expected,
			base+256,
			base+260,
		)
		status := readTestUint32(t, mem, base+256)
		if status != statusOK {
			t.Fatalf("put status = %d", status)
		}
		return readTestUint64(t, mem, base+260)
	}

	put([]byte("v1"), 0)
	put([]byte("v2"), 1)

	// CAS with wrong version should conflict.
	writeTestMemory(t, mem, base+128, []byte("v3"))
	h.stateCas(ctx, mod, uint64(tok),
		base, uint32(len(key)),
		base+128, uint32(len("v3")),
		3,
		base+256,
		base+260,
	)
	if readTestUint32(t, mem, base+256) != statusVersionConflict {
		t.Fatalf("expected version conflict, got %d", readTestUint32(t, mem, base+256))
	}
}

func TestHostImports_StateBatch(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("batch-plugin", 0, []string{"state:kv"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	ops, _ := json.Marshal([]stateBatchOp{
		{Op: "put", Key: "a", Value: json.RawMessage(`"1"`)},
		{Op: "put", Key: "b", Value: json.RawMessage(`"2"`)},
	})
	writeTestMemory(t, mem, base, ops)

	h.stateBatch(ctx, mod, uint64(tok),
		base, uint32(len(ops)),
		base+512,
		base+1024,
		512,
		base+516,
	)
	if readTestUint32(t, mem, base+512) != statusOK {
		t.Fatalf("batch status = %d", readTestUint32(t, mem, base+512))
	}
	resLen := readTestUint32(t, mem, base+516)
	var results []stateBatchResult
	resBytes, _ := mem.Read(base+1024, resLen)
	if err := json.Unmarshal(resBytes, &results); err != nil {
		t.Fatalf("unmarshal batch results: %v", err)
	}
	if len(results) != 2 || results[0].Status != statusOK || results[1].Status != statusOK {
		t.Fatalf("batch results = %+v", results)
	}
}

func TestHostImports_StorageStubUnauthorized(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Token has scoped storage:get:memories/* capability. The host
	// import should authorize it but, because no StorageClient is
	// wired in, return Unauthorized (the contract for "no backend
	// configured").
	tok, err := l.WASMRuntime().CapabilityTable().Issue("storage-plugin", 0, []string{"storage:get:memories/*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	bucket := []byte("memories")
	key := []byte("sessions/abc")
	writeTestMemory(t, mem, base, bucket)
	writeTestMemory(t, mem, base+64, key)
	h.storageGet(ctx, mod, uint64(tok),
		base, uint32(len(bucket)),
		base+64, uint32(len(key)),
		base+256, base+512, 128, base+260)
	if readTestUint32(t, mem, base+256) != statusUnauthorized {
		t.Fatalf("storage.get status = %d (expected Unauthorized because no StorageClient is wired in)",
			readTestUint32(t, mem, base+256))
	}
}

// fakeStorageClient is a minimal StorageClient used to verify the
// storage.put host import wires the plaintext body through to the
// gateway-backed client and surfaces the returned ETag.
type fakeStorageClient struct {
	gotBucket string
	gotKey    string
	gotBody   []byte
	gotUser   string
	etag      string
	err       error
}

func (f *fakeStorageClient) GetObject(ctx context.Context, bucket, key, originalUser string) ([]byte, error) {
	return nil, ErrNotFound
}

func (f *fakeStorageClient) PutObject(ctx context.Context, bucket, key string, body []byte, originalUser string) (string, error) {
	f.gotBucket = bucket
	f.gotKey = key
	f.gotBody = body
	f.gotUser = originalUser
	if f.err != nil {
		return "", f.err
	}
	return f.etag, nil
}

func TestHostImports_StoragePut(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	fake := &fakeStorageClient{etag: "abc123etag"}
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetStorageClient(fake)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Token with storage:put + scoped storage:put:memories/*.
	tok, err := l.WASMRuntime().CapabilityTable().Issue("storage-plugin", 0, []string{"storage:put", "storage:put:memories/*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	bucket := []byte("memories")
	key := []byte("sessions/abc")
	body := []byte(`{"hello":"world"}`)
	writeTestMemory(t, mem, base, bucket)
	writeTestMemory(t, mem, base+64, key)
	writeTestMemory(t, mem, base+128, body)

	// storagePut(token, bucket_ptr, bucket_len, key_ptr, key_len, body_ptr, body_len, out_status, out_etag_ptr, out_etag_cap, out_etag_len)
	h.storagePut(ctx, mod, uint64(tok),
		base, uint32(len(bucket)),
		base+64, uint32(len(key)),
		base+128, uint32(len(body)),
		base+256,
		base+512, 64,
		base+580,
	)

	if got := readTestUint32(t, mem, base+256); got != statusOK {
		t.Fatalf("storage.put status = %d (expected OK)", got)
	}
	if fake.gotBucket != "memories" || fake.gotKey != "sessions/abc" {
		t.Fatalf("storage.put forwarded bucket/key = %q/%q", fake.gotBucket, fake.gotKey)
	}
	if string(fake.gotBody) != string(body) {
		t.Fatalf("storage.put forwarded body = %q (expected %q)", fake.gotBody, body)
	}
	if fake.gotUser != "storage-plugin" {
		t.Fatalf("storage.put forwarded principal = %q (expected storage-plugin)", fake.gotUser)
	}
	etagLen := readTestUint32(t, mem, base+580)
	if etagLen != uint32(len(fake.etag)) {
		t.Fatalf("etag len = %d (expected %d)", etagLen, len(fake.etag))
	}
	etagBytes, _ := mem.Read(base+512, etagLen)
	if string(etagBytes) != fake.etag {
		t.Fatalf("etag = %q (expected %q)", string(etagBytes), fake.etag)
	}
}

func TestHostImports_StoragePut_UnauthorizedWithoutCapability(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	fake := &fakeStorageClient{etag: "abc"}
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetStorageClient(fake)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Token without storage:put.
	tok, err := l.WASMRuntime().CapabilityTable().Issue("no-put-plugin", 0, []string{"storage:get:memories/*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	bucket := []byte("memories")
	key := []byte("sessions/abc")
	body := []byte("data")
	writeTestMemory(t, mem, base, bucket)
	writeTestMemory(t, mem, base+64, key)
	writeTestMemory(t, mem, base+128, body)

	h.storagePut(ctx, mod, uint64(tok),
		base, uint32(len(bucket)),
		base+64, uint32(len(key)),
		base+128, uint32(len(body)),
		base+256,
		base+512, 64,
		base+580,
	)
	if got := readTestUint32(t, mem, base+256); got != statusUnauthorized {
		t.Fatalf("storage.put status = %d (expected Unauthorized without storage:put capability)", got)
	}
	if fake.gotBucket != "" {
		t.Fatalf("storage.put should not call PutObject when unauthorized")
	}
}

func TestHostImports_StoragePut_UnauthorizedWithoutScopedCap(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	fake := &fakeStorageClient{etag: "abc"}
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetStorageClient(fake)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Token with storage:put but scoped to a different bucket.
	tok, err := l.WASMRuntime().CapabilityTable().Issue("wrong-bucket-plugin", 0, []string{"storage:put", "storage:put:other/*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	bucket := []byte("memories")
	key := []byte("sessions/abc")
	body := []byte("data")
	writeTestMemory(t, mem, base, bucket)
	writeTestMemory(t, mem, base+64, key)
	writeTestMemory(t, mem, base+128, body)

	h.storagePut(ctx, mod, uint64(tok),
		base, uint32(len(bucket)),
		base+64, uint32(len(key)),
		base+128, uint32(len(body)),
		base+256,
		base+512, 64,
		base+580,
	)
	if got := readTestUint32(t, mem, base+256); got != statusUnauthorized {
		t.Fatalf("storage.put status = %d (expected Unauthorized without storage:put:memories/* scope)", got)
	}
}

func TestHostImports_StoragePut_NoStorageClient(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	// No SetStorageClient: contract is "no backend wired" => Unauthorized.
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("storage-plugin", 0, []string{"storage:put", "storage:put:memories/*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	bucket := []byte("memories")
	key := []byte("sessions/abc")
	body := []byte("data")
	writeTestMemory(t, mem, base, bucket)
	writeTestMemory(t, mem, base+64, key)
	writeTestMemory(t, mem, base+128, body)

	h.storagePut(ctx, mod, uint64(tok),
		base, uint32(len(bucket)),
		base+64, uint32(len(key)),
		base+128, uint32(len(body)),
		base+256,
		base+512, 64,
		base+580,
	)
	if got := readTestUint32(t, mem, base+256); got != statusUnauthorized {
		t.Fatalf("storage.put status = %d (expected Unauthorized when no StorageClient wired)", got)
	}
}

func TestHostImports_VectorWithoutCapability(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Token lacks vector:search.
	tok, err := l.WASMRuntime().CapabilityTable().Issue("no-vector", 0, []string{"state:kv"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	h.vectorSearch(ctx, mod, uint64(tok), 0, 0, 0, 0, 0, base, 0, 0, 0)
	if readTestUint32(t, mem, base) != statusUnauthorized {
		t.Fatalf("vector.search status = %d", readTestUint32(t, mem, base))
	}
}

func TestHostImports_VectorSearchMock(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	mock := &mockVectorSearcher{
		results: []vector.SearchResult{{ObjectKey: "obj1", Score: 0.95}},
	}
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), mock, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("vector-plugin", 0, []string{"vector:search:memories/*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	bucket := []byte("memories")
	writeTestMemory(t, mem, base, bucket)

	// Encode a 3-dimensional float32 query vector.
	vec := make([]byte, 12)
	binary.LittleEndian.PutUint32(vec[0:], 0)
	binary.LittleEndian.PutUint32(vec[4:], 0)
	binary.LittleEndian.PutUint32(vec[8:], 0)
	writeTestMemory(t, mem, base+128, vec)

	h.vectorSearch(ctx, mod, uint64(tok),
		base, uint32(len(bucket)),
		base+128, 3,
		10,
		base+256,
		base+1024,
		512,
		base+260,
	)
	if readTestUint32(t, mem, base+256) != statusOK {
		t.Fatalf("vector.search status = %d", readTestUint32(t, mem, base+256))
	}
	resLen := readTestUint32(t, mem, base+260)
	var got []struct {
		Key   string  `json:"key"`
		Score float32 `json:"score"`
	}
	resBytes, _ := mem.Read(base+1024, resLen)
	if err := json.Unmarshal(resBytes, &got); err != nil {
		t.Fatalf("unmarshal vector results: %v", err)
	}
	if len(got) != 1 || got[0].Key != "obj1" {
		t.Fatalf("vector results = %+v", got)
	}
}

type mockVectorSearcher struct {
	results []vector.SearchResult
}

func (m *mockVectorSearcher) Search(ctx context.Context, query vector.Vector, topK int, filters map[string]string) ([]vector.SearchResult, error) {
	return m.results, nil
}

// mockHTTPTransport intercepts requests and returns a fixed response or error.
type mockHTTPTransport struct {
	statusCode int
	body       []byte
	headers    map[string]string
	err        error
}

func (m *mockHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	h := make(http.Header)
	for k, v := range m.headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: m.statusCode,
		Status:     http.StatusText(m.statusCode),
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(m.body)),
		Request:    req,
	}, nil
}

// newMockHTTPFetcher returns an HTTPFetcher using a mock transport.
func newMockHTTPFetcher(status int, body []byte, headers map[string]string) *HTTPFetcher {
	f := NewHTTPFetcher()
	f.client.Transport = &mockHTTPTransport{statusCode: status, body: body, headers: headers}
	return f
}

// newMockHTTPFetcherErr returns an HTTPFetcher whose transport always errors.
func newMockHTTPFetcherErr(err error) *HTTPFetcher {
	f := NewHTTPFetcher()
	f.client.Transport = &mockHTTPTransport{err: err}
	return f
}

// writeTestString writes a string into guest memory and returns its ptr/len.
func writeTestString(t *testing.T, mem api.Memory, base uint32, s string) (uint32, uint32) {
	t.Helper()
	b := []byte(s)
	writeTestMemory(t, mem, base, b)
	return base, uint32(len(b))
}

func TestHostImports_EventPublishOwnership(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	// Create a session owned by "owner-plugin".
	sessionID := l.CreateSSESession("owner-plugin")

	// Owner publishes successfully.
	ownerTok, err := l.WASMRuntime().CapabilityTable().Issue("owner-plugin", 0, []string{"event:publish"}, neverExpires())
	if err != nil {
		t.Fatalf("issue owner token: %v", err)
	}

	const base uint32 = 64
	eventType := []byte("ping")
	payload := []byte(`{"seq":1}`)
	writeTestMemory(t, mem, base, eventType)
	writeTestMemory(t, mem, base+128, payload)

	h.routeEventPublish(ctx, mod, uint64(ownerTok), sessionID,
		base, uint32(len(eventType)),
		base+128, uint32(len(payload)),
		base+256,
	)
	if readTestUint32(t, mem, base+256) != statusOK {
		t.Fatalf("owner publish status = %d", readTestUint32(t, mem, base+256))
	}

	// Attacker plugin with event:publish but no ownership is rejected.
	attackerTok, err := l.WASMRuntime().CapabilityTable().Issue("attacker-plugin", 0, []string{"event:publish"}, neverExpires())
	if err != nil {
		t.Fatalf("issue attacker token: %v", err)
	}
	h.routeEventPublish(ctx, mod, uint64(attackerTok), sessionID,
		base, uint32(len(eventType)),
		base+128, uint32(len(payload)),
		base+256,
	)
	if readTestUint32(t, mem, base+256) != statusUnauthorized {
		t.Fatalf("attacker publish status = %d", readTestUint32(t, mem, base+256))
	}

	// Plugin without event:publish capability is rejected.
	noCapTok, err := l.WASMRuntime().CapabilityTable().Issue("no-cap-plugin", 0, []string{"state:kv"}, neverExpires())
	if err != nil {
		t.Fatalf("issue no-cap token: %v", err)
	}
	h.routeEventPublish(ctx, mod, uint64(noCapTok), sessionID,
		base, uint32(len(eventType)),
		base+128, uint32(len(payload)),
		base+256,
	)
	if readTestUint32(t, mem, base+256) != statusUnauthorized {
		t.Fatalf("no-cap publish status = %d", readTestUint32(t, mem, base+256))
	}
}

func TestHostImports_StateDelete(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("del-plugin", 0, []string{"state:kv"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	key := []byte("k")
	writeTestMemory(t, mem, base, key)

	_, err = l.StatePut("del-plugin", "k", []byte("v"), 0)
	if err != nil {
		t.Fatalf("state put: %v", err)
	}

	h.stateDelete(ctx, mod, uint64(tok), base, uint32(len(key)), base+256)
	if readTestUint32(t, mem, base+256) != statusOK {
		t.Fatalf("state.delete status = %d", readTestUint32(t, mem, base+256))
	}

	entry, err := l.StateGet("del-plugin", "k")
	if err != nil {
		t.Fatalf("state get: %v", err)
	}
	if entry != nil {
		t.Fatalf("expected key deleted, got %+v", entry)
	}
}

func TestHostImports_StateDeleteUnauthorized(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("no-state", 0, []string{"log:emit"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	h.stateDelete(ctx, mod, uint64(tok), 0, 0, 64)
	if readTestUint32(t, mem, 64) != statusUnauthorized {
		t.Fatalf("state.delete status = %d", readTestUint32(t, mem, 64))
	}
}

func TestHostImports_HookReadBodyModify(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("hook-plugin", 0, []string{"gateway:hook"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	h.OnModuleInstantiated(mod, tok)

	inv := &hookInvocation{
		pluginName: "hook-plugin",
		ctx: &pluginpb.HookContext{
			Operation:    "s3:GetObject",
			Phase:        "before",
			Bucket:       "b",
			Key:          "k",
			Method:       "GET",
			OriginalUser: "u",
			Headers:      map[string]string{"x-a": "1"},
			Body:         []byte("hello"),
			StatusCode:   0,
		},
	}
	handle := l.allocateHookContext(inv)

	const base uint32 = 64

	// hook.read
	h.hookRead(ctx, mod, handle, base, 1024, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("hook.read status = %d", readTestUint32(t, mem, base+260))
	}
	infoLen := readTestUint32(t, mem, base+256)
	infoBytes, _ := mem.Read(base, infoLen)
	var info map[string]any
	if err := json.Unmarshal(infoBytes, &info); err != nil {
		t.Fatalf("unmarshal hook info: %v", err)
	}
	if info["operation"] != "s3:GetObject" {
		t.Fatalf("hook info operation = %v", info["operation"])
	}

	// hook.body
	h.hookBody(ctx, mod, handle, base, 1024, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("hook.body status = %d", readTestUint32(t, mem, base+260))
	}
	bodyLen := readTestUint32(t, mem, base+256)
	bodyBytes, _ := mem.Read(base, bodyLen)
	if string(bodyBytes) != "hello" {
		t.Fatalf("hook.body = %q", bodyBytes)
	}

	// hook.modify: patch headers and body, short-circuit.
	patch := []byte(`{"headers":{"x-b":"2"},"body_b64":"d29ybGQ=","status_code":403,"short_circuit":true}`)
	writeTestMemory(t, mem, base, patch)
	h.hookModify(ctx, mod, handle, base, uint32(len(patch)), base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("hook.modify status = %d", readTestUint32(t, mem, base+260))
	}

	inv.mu.Lock()
	if !inv.shortCircuit || inv.abortStatus != 403 || string(inv.abortBody) != "world" {
		t.Fatalf("hook modify state short=%v status=%d body=%q", inv.shortCircuit, inv.abortStatus, inv.abortBody)
	}
	inv.mu.Unlock()

	// hook.short-circuit resets abort state.
	headers := []byte(`{"x-c":"3"}`)
	writeTestMemory(t, mem, base, headers)
	body := []byte("abort")
	writeTestMemory(t, mem, base+128, body)
	h.hookShortCircuit(ctx, mod, handle, 500, base, uint32(len(headers)), base+128, uint32(len(body)), base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("hook.short-circuit status = %d", readTestUint32(t, mem, base+260))
	}
	inv.mu.Lock()
	if inv.abortStatus != 500 || string(inv.abortBody) != "abort" || inv.abortHeaders["x-c"] != "3" {
		t.Fatalf("hook short-circuit state status=%d body=%q headers=%v", inv.abortStatus, inv.abortBody, inv.abortHeaders)
	}
	inv.mu.Unlock()
}

func TestHostImports_HookModifyInvalidBase64(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("hook-plugin", 0, []string{"gateway:hook"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	h.OnModuleInstantiated(mod, tok)

	inv := &hookInvocation{
		pluginName: "hook-plugin",
		ctx:        &pluginpb.HookContext{Operation: "s3:GetObject"},
	}
	handle := l.allocateHookContext(inv)

	const base uint32 = 64
	patch := []byte(`{"body_b64":"!!!"}`)
	writeTestMemory(t, mem, base, patch)
	h.hookModify(ctx, mod, handle, base, uint32(len(patch)), base+260)
	if readTestUint32(t, mem, base+260) != statusInvalidArgs {
		t.Fatalf("expected invalid args, got %d", readTestUint32(t, mem, base+260))
	}
}

func TestHostImports_HTTPFetch(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetHTTPFetcher(newMockHTTPFetcher(200, []byte("hi"), map[string]string{"x-r": "1"}))
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("net-plugin", TierTrusted, []string{"net:egress"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	// Directly register the validator to bypass plugin holder requirement.
	h.OnModuleInstantiated(mod, tok)
	h.modMu.Lock()
	h.moduleValidators[mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	h.modMu.Unlock()

	const base uint32 = 64
	methodPtr, methodLen := writeTestString(t, mem, base, "GET")
	urlPtr, urlLen := writeTestString(t, mem, base+32, "https://example.com/data")
	headersPtr, headersLen := writeTestString(t, mem, base+128, `{"Accept":"application/json"}`)

	h.httpFetch(ctx, mod, uint64(tok),
		methodPtr, methodLen,
		urlPtr, urlLen,
		headersPtr, headersLen,
		0, 0,
		1000,
		base+512,
		base+1024, 512,
		base+516,
	)
	if readTestUint32(t, mem, base+512) != statusOK {
		t.Fatalf("http.fetch status = %d", readTestUint32(t, mem, base+512))
	}
	respLen := readTestUint32(t, mem, base+516)
	respBytes, _ := mem.Read(base+1024, respLen)
	var resp FetchResult
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("resp status = %d", resp.Status)
	}
}

func TestHostImports_HTTPFetchURLDenied(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	h.SetHTTPFetcher(newMockHTTPFetcher(200, []byte("hi"), nil))
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("net-plugin", TierTrusted, []string{"net:egress"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	h.OnModuleInstantiated(mod, tok)
	h.modMu.Lock()
	h.moduleValidators[mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	h.modMu.Unlock()

	const base uint32 = 64
	methodPtr, methodLen := writeTestString(t, mem, base, "GET")
	urlPtr, urlLen := writeTestString(t, mem, base+32, "https://evil.com/data")

	h.httpFetch(ctx, mod, uint64(tok),
		methodPtr, methodLen,
		urlPtr, urlLen,
		0, 0,
		0, 0,
		0,
		base+512,
		base+1024, 512,
		base+516,
	)
	if readTestUint32(t, mem, base+512) != statusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", readTestUint32(t, mem, base+512))
	}
}

func TestHostImports_PipelineReadBodyWrite(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("pipe-plugin", 0, []string{"pipeline:step"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	inv := &pipelineInvocation{
		pluginName:   "pipe-plugin",
		key:          "obj",
		bucket:       "b",
		contentType:  "text/plain",
		userMetadata: map[string]string{"m": "1"},
		body:         []byte("input"),
	}
	handle := l.allocatePipelineContext(inv)

	const base uint32 = 64

	// pipeline.read
	h.pipelineRead(ctx, mod, uint64(tok), handle, base, 1024, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("pipeline.read status = %d", readTestUint32(t, mem, base+260))
	}

	// pipeline.body
	h.pipelineBody(ctx, mod, uint64(tok), handle, base, 1024, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("pipeline.body status = %d", readTestUint32(t, mem, base+260))
	}
	bodyLen := readTestUint32(t, mem, base+256)
	bodyBytes, _ := mem.Read(base, bodyLen)
	if string(bodyBytes) != "input" {
		t.Fatalf("pipeline.body = %q", bodyBytes)
	}

	// pipeline.output.write
	outBody := []byte("output")
	writeTestMemory(t, mem, base, outBody)
	ctPtr, ctLen := writeTestString(t, mem, base+128, "application/octet-stream")
	metaPtr, metaLen := writeTestString(t, mem, base+256, `{"m":"2"}`)
	h.pipelineOutputWrite(ctx, mod, uint64(tok), handle,
		base, uint32(len(outBody)),
		ctPtr, ctLen,
		metaPtr, metaLen,
		base+512,
	)
	if readTestUint32(t, mem, base+512) != statusOK {
		t.Fatalf("pipeline.output.write status = %d", readTestUint32(t, mem, base+512))
	}
	if string(inv.outputBody) != "output" || inv.outputContentType != "application/octet-stream" || inv.outputMetadata["m"] != "2" {
		t.Fatalf("pipeline output state = %+v", inv)
	}
}

func TestHostImports_EventSubscribeUnsubscribeRead(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("event-plugin", 0, []string{"event:subscribe"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	patPtr, patLen := writeTestString(t, mem, base, "user.*")

	h.eventSubscribe(ctx, mod, uint64(tok), patPtr, patLen, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("event.subscribe status = %d", readTestUint32(t, mem, base+260))
	}
	handle := readTestUint64(t, mem, base+256)

	// event.read needs an event invocation context.
	l.eventMu.Lock()
	l.eventContexts[handle] = &eventInvocation{pluginName: "event-plugin", payload: []byte(`{"x":1}`)}
	l.eventMu.Unlock()

	h.eventRead(ctx, mod, uint64(tok), handle, base, 1024, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("event.read status = %d", readTestUint32(t, mem, base+260))
	}
	readLen := readTestUint32(t, mem, base+256)
	readBytes, _ := mem.Read(base, readLen)
	if string(readBytes) != `{"x":1}` {
		t.Fatalf("event.read payload = %q", readBytes)
	}

	h.eventUnsubscribe(ctx, mod, uint64(tok), handle, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("event.unsubscribe status = %d", readTestUint32(t, mem, base+260))
	}
}

func TestHostImports_EventPublishToBus(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()
	l.SetEventBus(events.NewEventBus(2, 5*time.Second, t.TempDir(), 3, 100))

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("pub-plugin", 0, []string{"event:publish"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	ev := []byte(`{"event_type":"test.e","bucket":"b","key":"k"}`)
	writeTestMemory(t, mem, base, ev)

	h.eventPublish(ctx, mod, uint64(tok), base, uint32(len(ev)), base+256)
	if readTestUint32(t, mem, base+256) != statusOK {
		t.Fatalf("event.publish status = %d", readTestUint32(t, mem, base+256))
	}
}

func TestHostImports_TaskScheduleUnscheduleRead(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()
	testScheduler(t, l)

	manifest := minimalTaskManifest("task-plugin")
	ctx := testCtx(t)
	require.NoError(t, l.InstallPlugin(ctx, manifest, minimalPluginBytes, nil, ""))

	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("task-plugin", 0, []string{"task:schedule"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	cronPtr, cronLen := writeTestString(t, mem, base, "0 0 * * *")
	payload := []byte(`{"job":"backup"}`)
	writeTestMemory(t, mem, base+128, payload)

	h.taskSchedule(ctx, mod, uint64(tok), cronPtr, cronLen, base+128, uint32(len(payload)), base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("task.schedule status = %d", readTestUint32(t, mem, base+260))
	}
	scheduleHandle := readTestUint64(t, mem, base+256)

	// task.read uses an invocation handle (different from schedule handle).
	invHandle := l.allocateTaskContext(&taskInvocation{pluginName: "task-plugin", payload: []byte(`{"job":"backup"}`)})
	h.taskRead(ctx, mod, uint64(tok), invHandle, base, 1024, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("task.read status = %d", readTestUint32(t, mem, base+260))
	}

	h.taskUnschedule(ctx, mod, uint64(tok), scheduleHandle, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("task.unschedule status = %d", readTestUint32(t, mem, base+260))
	}
}

func TestHostImports_RouteResponseWrite(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("route-plugin", 0, []string{"http:route"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	// Allocate a request handle.
	l.reqMu.Lock()
	l.nextReqHandle++
	reqHandle := l.nextReqHandle
	l.requests[reqHandle] = &pluginRequest{method: "GET", path: "/x"}
	l.reqMu.Unlock()

	body := []byte("response")
	writeTestMemory(t, mem, base, body)
	headers := []byte(`{"Content-Type":"text/plain"}`)
	writeTestMemory(t, mem, base+128, headers)

	h.routeResponseWrite(ctx, mod, uint64(tok), reqHandle, 200,
		base+128, uint32(len(headers)),
		base, uint32(len(body)),
		base+256,
	)
	if readTestUint32(t, mem, base+256) != statusOK {
		t.Fatalf("route.response-write status = %d", readTestUint32(t, mem, base+256))
	}
}

func TestHostImports_RequestReadBody(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	const base uint32 = 64
	l.reqMu.Lock()
	l.nextReqHandle++
	reqHandle := l.nextReqHandle
	l.requests[reqHandle] = &pluginRequest{method: "POST", path: "/upload", body: []byte("data"), headers: map[string]string{"x": "1"}}
	l.reqMu.Unlock()

	h.requestRead(ctx, mod, reqHandle, base, 1024, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("request.read status = %d", readTestUint32(t, mem, base+260))
	}

	h.requestBody(ctx, mod, reqHandle, base, 1024, base+256, base+260)
	if readTestUint32(t, mem, base+260) != statusOK {
		t.Fatalf("request.body status = %d", readTestUint32(t, mem, base+260))
	}
	bodyLen := readTestUint32(t, mem, base+256)
	bodyBytes, _ := mem.Read(base, bodyLen)
	if string(bodyBytes) != "data" {
		t.Fatalf("request.body = %q", bodyBytes)
	}
}

func TestHostImports_Observability(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("obs-plugin", 0, []string{"log:emit", "metric:record", "trace:span"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	levelPtr, levelLen := writeTestString(t, mem, base, "info")
	msgPtr, msgLen := writeTestString(t, mem, base+32, `{"hello":"world"}`)
	h.logEmit(ctx, mod, uint64(tok), levelPtr, levelLen, msgPtr, msgLen)

	namePtr, nameLen := writeTestString(t, mem, base+128, "hits")
	labelsPtr, labelsLen := writeTestString(t, mem, base+160, `{"method":"GET"}`)
	h.metricRecord(ctx, mod, uint64(tok), namePtr, nameLen, 1.0, labelsPtr, labelsLen)

	spanNamePtr, spanNameLen := writeTestString(t, mem, base+256, "op")
	attrsPtr, attrsLen := writeTestString(t, mem, base+300, `{"k":"v"}`)
	h.traceSpanStart(ctx, mod, uint64(tok), spanNamePtr, spanNameLen, attrsPtr, attrsLen, base+400)
	h.traceSpanEnd(ctx, mod, uint64(tok), 0)
}

func TestHostImports_Setters(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)

	// Setters should accept nil values without panic.
	h.SetLogger(nil)
	h.SetHTTPFetcher(nil)
	h.SetPluginMetrics(nil)
	h.SetPluginTracer(nil)
}

func TestHostImports_StorageStubs(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("storage-plugin", 0, []string{"storage:get:*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	h.storagePut(ctx, mod, uint64(tok), base, 8, base+16, 8, base+32, 8, base+512, base+520, 32, base+524)
	if readTestUint32(t, mem, base+512) != statusUnauthorized {
		t.Fatalf("storage.put status = %d", readTestUint32(t, mem, base+512))
	}

	h.storageList(ctx, mod, uint64(tok), base, 8, base+16, 8, base+512, base+520, 32, base+524)
	if readTestUint32(t, mem, base+512) != statusUnauthorized {
		t.Fatalf("storage.list status = %d", readTestUint32(t, mem, base+512))
	}

	h.storageDelete(ctx, mod, uint64(tok), base, 8, base+16, 8, base+512)
	if readTestUint32(t, mem, base+512) != statusUnauthorized {
		t.Fatalf("storage.delete status = %d", readTestUint32(t, mem, base+512))
	}
}

func TestHostImports_VectorIndexRequiresIndexer(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("vector-plugin", 0, []string{"vector:index:*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	writeTestMemory(t, mem, base, []byte("memories"))
	writeTestMemory(t, mem, base+16, []byte("obj1"))
	vec := make([]byte, 16)
	writeTestMemory(t, mem, base+32, vec)

	h.vectorIndex(ctx, mod, uint64(tok), base, 8, base+16, 8, base+32, 4, base+512)
	if readTestUint32(t, mem, base+512) != statusInternal {
		t.Fatalf("vector.index status = %d", readTestUint32(t, mem, base+512))
	}
}

func TestHostImports_HTTPFetchErrors(t *testing.T) {
	const base uint32 = 64

	// Missing net:egress capability.
	e := newHostImportTestEnv(t, []string{"state:kv"})
	e.h.modMu.Lock()
	e.h.moduleValidators[e.mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	e.h.modMu.Unlock()
	e.h.httpFetch(e.ctx, e.mod, uint64(e.h.tokenForModule(e.mod)),
		base, 3, base+32, 24, 0, 0, 0, 0, 0,
		base+512, base+1024, 512, base+516,
	)
	require.Equal(t, statusUnauthorized, readTestUint32(t, e.mem, base+512))

	// httpFetcher not configured.
	e2 := newHostImportTestEnv(t, []string{"net:egress"})
	e2.h.modMu.Lock()
	e2.h.moduleValidators[e2.mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	e2.h.modMu.Unlock()
	methodPtr, methodLen := writeTestString(t, e2.mem, base, "GET")
	urlPtr, urlLen := writeTestString(t, e2.mem, base+32, "https://example.com/data")
	e2.h.httpFetch(e2.ctx, e2.mod, uint64(e2.h.tokenForModule(e2.mod)),
		methodPtr, methodLen, urlPtr, urlLen, 0, 0, 0, 0, 0,
		base+512, base+1024, 512, base+516,
	)
	require.Equal(t, statusInternal, readTestUint32(t, e2.mem, base+512))

	// Invalid method pointer.
	e3 := newHostImportTestEnv(t, []string{"net:egress"})
	e3.h.SetHTTPFetcher(newMockHTTPFetcher(200, []byte("ok"), nil))
	e3.h.modMu.Lock()
	e3.h.moduleValidators[e3.mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	e3.h.modMu.Unlock()
	e3.h.httpFetch(e3.ctx, e3.mod, uint64(e3.h.tokenForModule(e3.mod)),
		0xFFFFFFFF, 3, base+32, 24, 0, 0, 0, 0, 0,
		base+512, base+1024, 512, base+516,
	)
	require.Equal(t, statusInvalidArgs, readTestUint32(t, e3.mem, base+512))

	// Empty URL.
	e4 := newHostImportTestEnv(t, []string{"net:egress"})
	e4.h.SetHTTPFetcher(newMockHTTPFetcher(200, []byte("ok"), nil))
	e4.h.modMu.Lock()
	e4.h.moduleValidators[e4.mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	e4.h.modMu.Unlock()
	methodPtr, methodLen = writeTestString(t, e4.mem, base, "GET")
	e4.h.httpFetch(e4.ctx, e4.mod, uint64(e4.h.tokenForModule(e4.mod)),
		methodPtr, methodLen, base+32, 0, 0, 0, 0, 0, 0,
		base+512, base+1024, 512, base+516,
	)
	require.Equal(t, statusInvalidArgs, readTestUint32(t, e4.mem, base+512))

	// Invalid headers JSON.
	e5 := newHostImportTestEnv(t, []string{"net:egress"})
	e5.h.SetHTTPFetcher(newMockHTTPFetcher(200, []byte("ok"), nil))
	e5.h.modMu.Lock()
	e5.h.moduleValidators[e5.mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	e5.h.modMu.Unlock()
	methodPtr, methodLen = writeTestString(t, e5.mem, base, "GET")
	urlPtr, urlLen = writeTestString(t, e5.mem, base+32, "https://example.com/data")
	headersPtr, headersLen := writeTestString(t, e5.mem, base+128, "not-json")
	e5.h.httpFetch(e5.ctx, e5.mod, uint64(e5.h.tokenForModule(e5.mod)),
		methodPtr, methodLen, urlPtr, urlLen, headersPtr, headersLen, 0, 0, 0,
		base+512, base+1024, 512, base+516,
	)
	require.Equal(t, statusInvalidArgs, readTestUint32(t, e5.mem, base+512))

	// Fetch transport error.
	e6 := newHostImportTestEnv(t, []string{"net:egress"})
	e6.h.SetHTTPFetcher(newMockHTTPFetcherErr(errors.New("boom")))
	e6.h.modMu.Lock()
	e6.h.moduleValidators[e6.mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	e6.h.modMu.Unlock()
	methodPtr, methodLen = writeTestString(t, e6.mem, base, "GET")
	urlPtr, urlLen = writeTestString(t, e6.mem, base+32, "https://example.com/data")
	e6.h.httpFetch(e6.ctx, e6.mod, uint64(e6.h.tokenForModule(e6.mod)),
		methodPtr, methodLen, urlPtr, urlLen, 0, 0, 0, 0, 0,
		base+512, base+1024, 512, base+516,
	)
	require.Equal(t, statusInternal, readTestUint32(t, e6.mem, base+512))

	// Response buffer too small.
	e7 := newHostImportTestEnv(t, []string{"net:egress"})
	e7.h.SetHTTPFetcher(newMockHTTPFetcher(200, []byte("this response is larger than one byte"), nil))
	e7.h.modMu.Lock()
	e7.h.moduleValidators[e7.mod] = NewNetworkEgressValidator(TierTrusted, []string{"https://example.com/*"})
	e7.h.modMu.Unlock()
	methodPtr, methodLen = writeTestString(t, e7.mem, base, "GET")
	urlPtr, urlLen = writeTestString(t, e7.mem, base+32, "https://example.com/data")
	e7.h.httpFetch(e7.ctx, e7.mod, uint64(e7.h.tokenForModule(e7.mod)),
		methodPtr, methodLen, urlPtr, urlLen, 0, 0, 0, 0, 0,
		base+512, base+1024, 1, base+516,
	)
	require.Equal(t, statusBufferTooSmall, readTestUint32(t, e7.mem, base+512))
	require.Greater(t, readTestUint32(t, e7.mem, base+516), uint32(0))

	// Tier 0 plugin denied egress even with matching pattern.
	e8 := newHostImportTestEnv(t, []string{"net:egress"})
	e8.h.SetHTTPFetcher(newMockHTTPFetcher(200, []byte("ok"), nil))
	e8.h.modMu.Lock()
	e8.h.moduleValidators[e8.mod] = NewNetworkEgressValidator(TierUntrusted, nil)
	e8.h.modMu.Unlock()
	methodPtr, methodLen = writeTestString(t, e8.mem, base, "GET")
	urlPtr, urlLen = writeTestString(t, e8.mem, base+32, "https://example.com/data")
	e8.h.httpFetch(e8.ctx, e8.mod, uint64(e8.h.tokenForModule(e8.mod)),
		methodPtr, methodLen, urlPtr, urlLen, 0, 0, 0, 0, 0,
		base+512, base+1024, 512, base+516,
	)
	require.Equal(t, statusUnauthorized, readTestUint32(t, e8.mem, base+512))
}

func TestHostImports_FloatHelpers(t *testing.T) {
	bits := float64ToUint64(1.5)
	if uint64ToFloat64(bits) != 1.5 {
		t.Fatal("float64ToUint64 / uint64ToFloat64 mismatch")
	}
	if intToFloat64(42) != 42.0 {
		t.Fatal("intToFloat64 mismatch")
	}
	v, err := parseFloat64("3.14")
	if err != nil || v != 3.14 {
		t.Fatalf("parseFloat64 = %v, %v", v, err)
	}
}
