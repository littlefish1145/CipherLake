package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testBackendInterface runs a comprehensive set of tests against any BackendStorage
// implementation to verify it satisfies the interface contract.
func testBackendInterface(t *testing.T, backend BackendStorage, name string) {
	ctx := context.Background()

	t.Run(name+"_PutAndGet", func(t *testing.T) {
		testData := []byte("hello world test data")
		err := backend.Put(ctx, "test/key1", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		reader, err := backend.Get(ctx, "test/key1")
		require.NoError(t, err)
		defer reader.Close()

		gotData, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, testData, gotData)
	})

	t.Run(name+"_GetNotFound", func(t *testing.T) {
		_, err := backend.Get(ctx, "nonexistent/key")
		assert.Error(t, err)
	})

	t.Run(name+"_Delete", func(t *testing.T) {
		testData := []byte("delete me")
		err := backend.Put(ctx, "test/delete", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		err = backend.Delete(ctx, "test/delete")
		require.NoError(t, err)

		_, err = backend.Get(ctx, "test/delete")
		assert.Error(t, err)
	})

	t.Run(name+"_DeleteIdempotent", func(t *testing.T) {
		// Deleting a non-existent key should not error
		err := backend.Delete(ctx, "test/nonexistent_delete")
		require.NoError(t, err)
	})

	t.Run(name+"_Exists", func(t *testing.T) {
		exists, err := backend.Exists(ctx, "test/exists_check")
		require.NoError(t, err)
		assert.False(t, exists)

		testData := []byte("exists test")
		err = backend.Put(ctx, "test/exists_check", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		exists, err = backend.Exists(ctx, "test/exists_check")
		require.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run(name+"_Size", func(t *testing.T) {
		testData := []byte("size check data")
		err := backend.Put(ctx, "test/size_check", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		size, err := backend.Size(ctx, "test/size_check")
		require.NoError(t, err)
		assert.Equal(t, int64(len(testData)), size)
	})

	t.Run(name+"_SizeNotFound", func(t *testing.T) {
		_, err := backend.Size(ctx, "test/nonexistent_size")
		assert.Error(t, err)
	})

	t.Run(name+"_PutReader", func(t *testing.T) {
		testData := []byte("put reader data")
		etag, err := backend.PutReader(ctx, "test/putreader", bytes.NewReader(testData))
		require.NoError(t, err)
		assert.NotEmpty(t, etag)

		reader, err := backend.Get(ctx, "test/putreader")
		require.NoError(t, err)
		defer reader.Close()

		gotData, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, testData, gotData)
	})

	t.Run(name+"_GetRange", func(t *testing.T) {
		testData := []byte("0123456789ABCDEF")
		err := backend.Put(ctx, "test/range", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		reader, err := backend.GetRange(ctx, "test/range", 4, 6)
		require.NoError(t, err)
		defer reader.Close()

		gotData, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, testData[4:10], gotData)
	})

	t.Run(name+"_GetRangeNotFound", func(t *testing.T) {
		_, err := backend.GetRange(ctx, "test/nonexistent_range", 0, 10)
		assert.Error(t, err)
	})

	t.Run(name+"_AtomicRename", func(t *testing.T) {
		testData := []byte("rename test data")
		err := backend.Put(ctx, "test/rename_old", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		err = backend.AtomicRename(ctx, "test/rename_old", "test/rename_new")
		require.NoError(t, err)

		// Old path should not exist
		_, err = backend.Get(ctx, "test/rename_old")
		assert.Error(t, err)

		// New path should have the data
		reader, err := backend.Get(ctx, "test/rename_new")
		require.NoError(t, err)
		defer reader.Close()

		gotData, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, testData, gotData)
	})

	t.Run(name+"_List", func(t *testing.T) {
		prefix := fmt.Sprintf("test/list_%s/", name)
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("%sitem%d", prefix, i)
			err := backend.Put(ctx, key, bytes.NewReader([]byte{byte(i)}), 1)
			require.NoError(t, err)
		}

		keys, err := backend.List(ctx, prefix)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(keys), 3)
	})

	t.Run(name+"_Close", func(t *testing.T) {
		err := backend.Close()
		require.NoError(t, err)
	})

	t.Run(name+"_Name", func(t *testing.T) {
		n := backend.Name()
		assert.NotEmpty(t, n)
	})
}

func TestFileBackend_Interface(t *testing.T) {
	tmpDir := t.TempDir()
	backend, err := NewFileBackend(tmpDir)
	require.NoError(t, err)
	testBackendInterface(t, backend, "file")
}

func TestMemoryBackend_Interface(t *testing.T) {
	backend := newMemoryBackend("test-mem")
	testBackendInterface(t, backend, "memory")
}

func TestErasureBackend_Interface(t *testing.T) {
	backends := make([]BackendStorage, 6)
	for i := 0; i < 6; i++ {
		backends[i] = newMemoryBackend(fmt.Sprintf("erasure-mem%d", i))
	}

	eb, err := NewErasureCodedBackend(ErasureConfig{
		DataShards:   4,
		ParityShards: 2,
		Backends:     backends,
	})
	require.NoError(t, err)

	// Note: erasure backend Size() returns approximate value, so we skip exact size check
	ctx := context.Background()

	t.Run("erasure_PutAndGet", func(t *testing.T) {
		testData := []byte("erasure interface test data")
		err := eb.Put(ctx, "itest/key1", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		reader, err := eb.Get(ctx, "itest/key1")
		require.NoError(t, err)
		defer reader.Close()

		gotData, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, testData, gotData)
	})

	t.Run("erasure_Exists", func(t *testing.T) {
		exists, err := eb.Exists(ctx, "itest/key1")
		require.NoError(t, err)
		assert.True(t, exists)

		exists, err = eb.Exists(ctx, "itest/nonexistent")
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("erasure_Delete", func(t *testing.T) {
		testData := []byte("delete me")
		err := eb.Put(ctx, "itest/delete", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		err = eb.Delete(ctx, "itest/delete")
		require.NoError(t, err)

		_, err = eb.Get(ctx, "itest/delete")
		assert.Error(t, err)
	})

	t.Run("erasure_PutReader", func(t *testing.T) {
		testData := []byte("put reader test")
		etag, err := eb.PutReader(ctx, "itest/putreader", bytes.NewReader(testData))
		require.NoError(t, err)
		assert.NotEmpty(t, etag)
	})

	t.Run("erasure_GetRange", func(t *testing.T) {
		testData := []byte("0123456789ABCDEF")
		err := eb.Put(ctx, "itest/range", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		reader, err := eb.GetRange(ctx, "itest/range", 4, 6)
		require.NoError(t, err)
		defer reader.Close()

		gotData, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, testData[4:10], gotData)
	})

	t.Run("erasure_AtomicRename", func(t *testing.T) {
		testData := []byte("rename test")
		err := eb.Put(ctx, "itest/old", bytes.NewReader(testData), int64(len(testData)))
		require.NoError(t, err)

		err = eb.AtomicRename(ctx, "itest/old", "itest/new")
		require.NoError(t, err)

		reader, err := eb.Get(ctx, "itest/new")
		require.NoError(t, err)
		defer reader.Close()

		gotData, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, testData, gotData)
	})

	t.Run("erasure_List", func(t *testing.T) {
		keys, err := eb.List(ctx, "itest/")
		require.NoError(t, err)
		assert.NotEmpty(t, keys)
	})

	t.Run("erasure_Name", func(t *testing.T) {
		assert.Equal(t, "erasure", eb.Name())
	})

	t.Run("erasure_Close", func(t *testing.T) {
		err := eb.Close()
		require.NoError(t, err)
	})
}

func TestFileBackend_PutReader_AtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	backend, err := NewFileBackend(tmpDir)
	require.NoError(t, err)
	ctx := context.Background()

	testData := []byte("atomic write test")
	etag, err := backend.PutReader(ctx, "test/atomic", bytes.NewReader(testData))
	require.NoError(t, err)
	assert.NotEmpty(t, etag)

	// Verify no temp files left behind
	err = filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		if len(base) >= 4 && base[:4] == ".tmp" {
			t.Errorf("temp file should not remain: %s", path)
		}
		return nil
	})
	require.NoError(t, err)
}

func TestFileBackend_GetRange(t *testing.T) {
	tmpDir := t.TempDir()
	backend, err := NewFileBackend(tmpDir)
	require.NoError(t, err)
	ctx := context.Background()

	testData := []byte("0123456789ABCDEF")
	err = backend.Put(ctx, "test/range", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	reader, err := backend.GetRange(ctx, "test/range", 4, 6)
	require.NoError(t, err)
	defer reader.Close()

	gotData, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData[4:10], gotData)
}

func TestFileBackend_AtomicRename(t *testing.T) {
	tmpDir := t.TempDir()
	backend, err := NewFileBackend(tmpDir)
	require.NoError(t, err)
	ctx := context.Background()

	testData := []byte("rename test")
	err = backend.Put(ctx, "test/old", bytes.NewReader(testData), int64(len(testData)))
	require.NoError(t, err)

	err = backend.AtomicRename(ctx, "test/old", "test/new")
	require.NoError(t, err)

	_, err = backend.Get(ctx, "test/old")
	assert.Error(t, err)

	reader, err := backend.Get(ctx, "test/new")
	require.NoError(t, err)
	defer reader.Close()

	gotData, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, gotData)
}

func TestS3Backend_InterfaceType(t *testing.T) {
	var _ BackendStorage = (*S3Backend)(nil)
}

func TestAzureBlobBackend_InterfaceType(t *testing.T) {
	var _ BackendStorage = (*AzureBlobBackend)(nil)
}

func TestErasureCodedBackend_InterfaceType(t *testing.T) {
	var _ BackendStorage = (*ErasureCodedBackend)(nil)
}
