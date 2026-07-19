package pluginloader

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"cipherlake/internal/config"
	"cipherlake/internal/vector"
)

// newTestLoader creates a Loader backed by a temporary Raft data directory.
// The caller must call Shutdown on the returned loader.
func newTestLoader(t *testing.T) *Loader {
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
func newTestMemoryModule(t *testing.T, ctx context.Context, rt wazero.Runtime) api.Module {
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
//   (module (memory 1) (export "memory" (memory 0)))
var memoryModuleBytes = []byte{
	0x00, 0x61, 0x73, 0x6d, // magic
	0x01, 0x00, 0x00, 0x00, // version
	0x05, 0x03, 0x01, 0x00, 0x01, // memory section: 1 memory, 0, 1 page
	0x07, 0x0a, 0x01, // export section (size=10, count=1)
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, // "memory"
	0x02, 0x00, // memory export, index 0
}

func writeTestMemory(t *testing.T, mem api.Memory, ptr uint32, data []byte) {
	t.Helper()
	if !mem.Write(ptr, data) {
		t.Fatalf("failed to write test memory at %d", ptr)
	}
}

func readTestUint32(t *testing.T, mem api.Memory, ptr uint32) uint32 {
	t.Helper()
	b, ok := mem.Read(ptr, 4)
	if !ok {
		t.Fatalf("failed to read u32 at %d", ptr)
	}
	return binary.LittleEndian.Uint32(b)
}

func readTestUint64(t *testing.T, mem api.Memory, ptr uint32) uint64 {
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
		base + 128, uint32(len(value)),
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
		base + 512, 128,
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
			base + 128, uint32(len(v)),
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
		base + 128, uint32(len("v3")),
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

	tok, err := l.WASMRuntime().CapabilityTable().Issue("storage-plugin", 0, []string{"storage:get:memories/*"}, neverExpires())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	const base uint32 = 64
	h.storageGet(ctx, mod, uint64(tok), 0, 0, 0, 0, base, 0, 0, 0)
	if readTestUint32(t, mem, base) != statusUnauthorized {
		t.Fatalf("storage.get status = %d", readTestUint32(t, mem, base))
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
		base + 128, 3,
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
