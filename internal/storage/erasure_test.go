package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memoryBackend is an in-memory BackendStorage for testing.
type memoryBackend struct {
	mu    sync.RWMutex
	data  map[string][]byte
	name  string
	// failPut causes Put to fail for specific shard indices (for fault injection)
	failPut map[int]bool
}

func newMemoryBackend(name string) *memoryBackend {
	return &memoryBackend{
		data:    make(map[string][]byte),
		name:    name,
		failPut: make(map[int]bool),
	}
}

func (m *memoryBackend) Name() string { return m.name }

func (m *memoryBackend) Put(ctx context.Context, path string, data io.Reader, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	m.data[path] = all
	return nil
}

func (m *memoryBackend) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.data[path]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}

func (m *memoryBackend) Delete(ctx context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, path)
	return nil
}

func (m *memoryBackend) Exists(ctx context.Context, path string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.data[path]
	return ok, nil
}

func (m *memoryBackend) Size(ctx context.Context, path string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.data[path]
	if !ok {
		return 0, ErrObjectNotFound
	}
	return int64(len(d)), nil
}

func (m *memoryBackend) List(ctx context.Context, prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var keys []string
	for k := range m.data {
		keys = append(keys, k)
	}
	return keys, nil
}

func (m *memoryBackend) PutReader(ctx context.Context, path string, reader io.Reader) (string, error) {
	err := m.Put(ctx, path, reader, 0)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`"%x"`, len(m.data[path])), nil
}

func (m *memoryBackend) GetRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.data[path]
	if !ok {
		return nil, ErrObjectNotFound
	}
	if offset > int64(len(d)) {
		return nil, fmt.Errorf("offset beyond data")
	}
	end := offset + length
	if end > int64(len(d)) {
		end = int64(len(d))
	}
	return io.NopCloser(bytes.NewReader(d[offset:end])), nil
}

func (m *memoryBackend) AtomicRename(ctx context.Context, oldPath, newPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[oldPath]
	if !ok {
		return ErrObjectNotFound
	}
	m.data[newPath] = d
	delete(m.data, oldPath)
	return nil
}

func (m *memoryBackend) Close() error { return nil }

func TestErasureBackend_PutAndGet(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()
	testData := []byte("Hello, erasure coding world! This is a test of the Reed-Solomon erasure coding backend.")

	// Put
	err = eb.Put(ctx, "test/key1", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	// Get
	reader, err := eb.Get(ctx, "test/key1")
	require.NoError(t, err)
	defer reader.Close()

	gotData, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, gotData)
}

func TestErasureBackend_TolerateFailures(t *testing.T) {
	backends := make([]BackendStorage, 6)
	memBackends := make([]*memoryBackend, 6)
	for i := 0; i < 6; i++ {
		memBackends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
		backends[i] = memBackends[i]
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()
	testData := []byte("Testing fault tolerance: this data should survive even if some backends fail!")

	// Put data
	err = eb.Put(ctx, "test/fault", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	// Simulate failure of 2 backends (shards 1 and 3)
	delete(memBackends[1].data, "test/fault.shard.1")
	delete(memBackends[3].data, "test/fault.shard.3")

	// Should still be able to reconstruct
	reader, err := eb.Get(ctx, "test/fault")
	require.NoError(t, err)
	defer reader.Close()

	gotData, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, gotData)
}

func TestErasureBackend_Delete(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()
	testData := []byte("delete me")

	err = eb.Put(ctx, "test/del", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	err = eb.Delete(ctx, "test/del")
	require.NoError(t, err)

	_, err = eb.Get(ctx, "test/del")
	assert.Error(t, err)
}

func TestErasureBackend_Exists(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()

	exists, err := eb.Exists(ctx, "test/notexist")
	require.NoError(t, err)
	assert.False(t, exists)

	testData := []byte("exists test")
	err = eb.Put(ctx, "test/exists", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	exists, err = eb.Exists(ctx, "test/exists")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestErasureBackend_GetRange(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()
	testData := []byte("0123456789ABCDEF")

	err = eb.Put(ctx, "test/range", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	reader, err := eb.GetRange(ctx, "test/range", 4, 6)
	require.NoError(t, err)
	defer reader.Close()

	gotData, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData[4:10], gotData)
}

func TestErasureBackend_PutReader(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()
	testData := []byte("put reader test data")

	etag, err := eb.PutReader(ctx, "test/putreader", bytes.NewReader(testData))
	require.NoError(t, err)
	assert.NotEmpty(t, etag)

	reader, err := eb.Get(ctx, "test/putreader")
	require.NoError(t, err)
	defer reader.Close()

	gotData, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, gotData)
}

func TestErasureBackend_AtomicRename(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()
	testData := []byte("rename test")

	err = eb.Put(ctx, "test/old", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	err = eb.AtomicRename(ctx, "test/old", "test/new")
	require.NoError(t, err)

	// Old path should not exist
	_, err = eb.Get(ctx, "test/old")
	assert.Error(t, err)

	// New path should have the data
	reader, err := eb.Get(ctx, "test/new")
	require.NoError(t, err)
	defer reader.Close()

	gotData, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, gotData)
}

func TestErasureBackend_NotEnoughBackends(t *testing.T) {
	backends := make([]BackendStorage, 4)
	for i := 0; i < 4; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	_, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at least 6 backends")
}

func TestErasureBackend_DefaultConfig(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	// Test with default values (0 should become 4 and 2)
	eb, err := NewErasureCodedBackend(ErasureConfig{
		Backends: backends,
	})
	require.NoError(t, err)
	assert.Equal(t, 4, eb.dataShards)
	assert.Equal(t, 2, eb.parityShards)
}

func TestErasureBackend_LargeData(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()
	// Create data larger than one shard
	testData := make([]byte, 4096)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	err = eb.Put(ctx, "test/large", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	reader, err := eb.Get(ctx, "test/large")
	require.NoError(t, err)
	defer reader.Close()

	gotData, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, gotData)
}

func TestErasureBackend_Size(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()
	testData := []byte("size test data")

	err = eb.Put(ctx, "test/size", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	size, err := eb.Size(ctx, "test/size")
	require.NoError(t, err)
	// Size is approximate (shard_size * dataShards)
	assert.Greater(t, size, int64(0))
}

func TestErasureBackend_List(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	ctx := context.Background()

	err = eb.Put(ctx, "list/a", bytes.NewReader([]byte("a")), 1)
	require.NoError(t, err)
	err = eb.Put(ctx, "list/b", bytes.NewReader([]byte("b")), 1)
	require.NoError(t, err)

	keys, err := eb.List(ctx, "list/")
	require.NoError(t, err)
	assert.Len(t, keys, 2)
}
