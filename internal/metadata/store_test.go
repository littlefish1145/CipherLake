package metadata

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBoltDBMetadataStore(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	require.NotNil(t, store)

	err = store.Close()
	assert.NoError(t, err)
}

func TestBoltDBMetadataStore_CreateBucket(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	bucket := &BucketInfo{
		Name:      "test-bucket",
		OwnerID:   "user-001",
		OwnerName: "test-user",
		Region:    "us-east-1",
	}

	err = store.CreateBucket(ctx, "test-bucket", bucket)
	assert.NoError(t, err)

	retrieved, err := store.GetBucket(ctx, "test-bucket")
	assert.NoError(t, err)
	assert.Equal(t, "test-bucket", retrieved.Name)
	assert.Equal(t, "user-001", retrieved.OwnerID)
}

func TestBoltDBMetadataStore_DeleteBucket(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	bucket := &BucketInfo{
		Name:      "test-bucket",
		OwnerID:   "user-001",
		OwnerName: "test-user",
		Region:    "us-east-1",
	}

	err = store.CreateBucket(ctx, "test-bucket", bucket)
	require.NoError(t, err)

	err = store.DeleteBucket(ctx, "test-bucket")
	assert.NoError(t, err)

	_, err = store.GetBucket(ctx, "test-bucket")
	assert.Error(t, err)
}

func TestBoltDBMetadataStore_DeleteBucketUpdatesStats(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	err = store.CreateBucket(ctx, "test-bucket", &BucketInfo{Name: "test-bucket", OwnerID: "user-001"})
	require.NoError(t, err)

	err = store.PutObject(ctx, "test-bucket", "a.txt", &ObjectMetadata{
		Key:    "a.txt",
		Bucket: "test-bucket",
		Size:   10,
	})
	require.NoError(t, err)
	err = store.PutObject(ctx, "test-bucket", "b.txt", &ObjectMetadata{
		Key:    "b.txt",
		Bucket: "test-bucket",
		Size:   15,
	})
	require.NoError(t, err)
	err = store.PutObjectVersion(ctx, "test-bucket", "a.txt", &ObjectMetadata{
		Key:       "a.txt",
		Bucket:    "test-bucket",
		Size:      10,
		VersionID: "version-1",
	})
	require.NoError(t, err)
	err = store.PutUpload(ctx, &MultipartUpload{Bucket: "test-bucket", Key: "pending.txt", UploadID: "upload-1"})
	require.NoError(t, err)
	err = store.AddPart(ctx, "upload-1", &UploadPart{PartNumber: 1, Size: 5})
	require.NoError(t, err)
	err = store.PutResumableSession(ctx, &ResumableSession{UploadID: "resumable-1", Bucket: "test-bucket", Key: "pending.bin"})
	require.NoError(t, err)

	stats := store.GetStats()
	assert.Equal(t, int64(1), stats.BucketCount)
	assert.Equal(t, int64(2), stats.ObjectCount)
	assert.Equal(t, int64(25), stats.TotalSize)
	assert.Equal(t, int64(1), stats.UploadCount)

	err = store.DeleteBucket(ctx, "test-bucket")
	require.NoError(t, err)
	stats = store.GetStats()
	assert.Equal(t, int64(0), stats.BucketCount)
	assert.Equal(t, int64(0), stats.ObjectCount)
	assert.Equal(t, int64(0), stats.TotalSize)
	assert.Equal(t, int64(0), stats.UploadCount)
	_, err = store.GetObjectVersion(ctx, "test-bucket", "a.txt", "version-1")
	assert.ErrorIs(t, err, ErrVersionNotFound)
	_, err = store.GetUpload(ctx, "test-bucket", "pending.txt", "upload-1")
	assert.ErrorIs(t, err, ErrKeyNotFound)
	parts, err := store.GetParts(ctx, "upload-1")
	require.NoError(t, err)
	assert.Empty(t, parts)
	_, err = store.GetResumableSession(ctx, "resumable-1")
	assert.ErrorIs(t, err, ErrKeyNotFound)

	err = store.DeleteBucket(ctx, "test-bucket")
	require.NoError(t, err)
	stats = store.GetStats()
	assert.Equal(t, int64(0), stats.BucketCount)
}

func TestBoltDBMetadataStore_PutObject(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	bucket := &BucketInfo{Name: "test-bucket", OwnerID: "user-001"}
	err = store.CreateBucket(ctx, "test-bucket", bucket)
	require.NoError(t, err)

	obj := &ObjectMetadata{
		Key:         "test/key.txt",
		Bucket:      "test-bucket",
		Size:        1024,
		ContentType: "text/plain",
		ETag:        "abc123",
	}

	err = store.PutObject(ctx, "test-bucket", "test/key.txt", obj)
	assert.NoError(t, err)
}

func TestBoltDBMetadataStore_PutObjectReturnsWALError(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewBoltDBMetadataStore(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer store.Close()

	store.walPath = filepath.Join(tmpDir, "missing", "metadata.wal")
	store.walBuf = make([]WALEntry, 99)

	err = store.PutObject(context.Background(), "test-bucket", "test.txt", &ObjectMetadata{
		Key:    "test.txt",
		Bucket: "test-bucket",
		Size:   1,
	})
	require.Error(t, err)

	_, err = store.GetObject(context.Background(), "test-bucket", "test.txt")
	assert.ErrorIs(t, err, ErrKeyNotFound)
}

func TestBoltDBMetadataStore_GetObject(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	bucket := &BucketInfo{Name: "test-bucket", OwnerID: "user-001"}
	err = store.CreateBucket(ctx, "test-bucket", bucket)
	require.NoError(t, err)

	obj := &ObjectMetadata{
		Key:         "test/key.txt",
		Bucket:      "test-bucket",
		Size:        1024,
		ContentType: "text/plain",
		ETag:        "abc123",
	}
	err = store.PutObject(ctx, "test-bucket", "test/key.txt", obj)
	require.NoError(t, err)

	retrieved, err := store.GetObject(ctx, "test-bucket", "test/key.txt")
	assert.NoError(t, err)
	assert.Equal(t, "test/key.txt", retrieved.Key)
	assert.Equal(t, int64(1024), retrieved.Size)
}

func TestBoltDBMetadataStore_DeleteObject(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	bucket := &BucketInfo{Name: "test-bucket", OwnerID: "user-001"}
	err = store.CreateBucket(ctx, "test-bucket", bucket)
	require.NoError(t, err)

	obj := &ObjectMetadata{
		Key:    "test/key.txt",
		Bucket: "test-bucket",
		Size:   1024,
	}
	err = store.PutObject(ctx, "test-bucket", "test/key.txt", obj)
	require.NoError(t, err)

	err = store.DeleteObject(ctx, "test-bucket", "test/key.txt")
	assert.NoError(t, err)

	_, err = store.GetObject(ctx, "test-bucket", "test/key.txt")
	assert.Error(t, err)
}

// TestBoltDBMetadataStore_DeleteObjectAlsoRemovesVersionRecords verifies that
// deleting the active object also removes any persisted version records for
// the same key, preventing orphaned version metadata.
func TestBoltDBMetadataStore_DeleteObjectAlsoRemovesVersionRecords(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	require.NoError(t, store.CreateBucket(ctx, "test-bucket", &BucketInfo{Name: "test-bucket", OwnerID: "user-001"}))

	require.NoError(t, store.PutObject(ctx, "test-bucket", "key.txt", &ObjectMetadata{
		Key:    "key.txt",
		Bucket: "test-bucket",
		Size:   10,
	}))
	require.NoError(t, store.PutObjectVersion(ctx, "test-bucket", "key.txt", &ObjectMetadata{
		Key:       "key.txt",
		Bucket:    "test-bucket",
		Size:      10,
		VersionID: "v1",
	}))
	require.NoError(t, store.PutObjectVersion(ctx, "test-bucket", "key.txt", &ObjectMetadata{
		Key:       "key.txt",
		Bucket:    "test-bucket",
		Size:      10,
		VersionID: "v2",
	}))

	versions, err := store.ListObjectVersions(ctx, "test-bucket", "key.txt", 10)
	require.NoError(t, err)
	require.Len(t, versions, 2)

	require.NoError(t, store.DeleteObject(ctx, "test-bucket", "key.txt"))

	// Both version records should be gone after deleting the active object.
	versions, err = store.ListObjectVersions(ctx, "test-bucket", "key.txt", 10)
	require.NoError(t, err)
	assert.Empty(t, versions)

	_, err = store.GetObjectVersion(ctx, "test-bucket", "key.txt", "v1")
	assert.ErrorIs(t, err, ErrVersionNotFound)
	_, err = store.GetObjectVersion(ctx, "test-bucket", "key.txt", "v2")
	assert.ErrorIs(t, err, ErrVersionNotFound)
}

func TestBoltDBMetadataStore_PutObjectOverwriteUpdatesStatsByDelta(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	bucket := &BucketInfo{Name: "test-bucket", OwnerID: "user-001"}
	err = store.CreateBucket(ctx, "test-bucket", bucket)
	require.NoError(t, err)

	err = store.PutObject(ctx, "test-bucket", "test/key.txt", &ObjectMetadata{
		Key:    "test/key.txt",
		Bucket: "test-bucket",
		Size:   10,
	})
	require.NoError(t, err)

	err = store.PutObject(ctx, "test-bucket", "test/key.txt", &ObjectMetadata{
		Key:    "test/key.txt",
		Bucket: "test-bucket",
		Size:   25,
	})
	require.NoError(t, err)

	gotBucket, err := store.GetBucket(ctx, "test-bucket")
	require.NoError(t, err)
	assert.Equal(t, int64(1), gotBucket.ObjectCount)
	assert.Equal(t, int64(25), gotBucket.TotalSize)

	stats := store.GetStats()
	assert.Equal(t, int64(1), stats.ObjectCount)
	assert.Equal(t, int64(25), stats.TotalSize)
}

func TestBoltDBMetadataStore_DeleteMissingObjectDoesNotChangeStats(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	err = store.CreateBucket(ctx, "test-bucket", &BucketInfo{Name: "test-bucket", OwnerID: "user-001"})
	require.NoError(t, err)

	before := store.GetStats()
	err = store.DeleteObject(ctx, "test-bucket", "missing.txt")
	require.NoError(t, err)
	after := store.GetStats()

	assert.Equal(t, before.ObjectCount, after.ObjectCount)
	assert.Equal(t, before.TotalSize, after.TotalSize)
}

func TestBoltDBMetadataStore_PutUploadOverwriteDoesNotIncrementUploadCount(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	upload := &MultipartUpload{
		UploadID: "upload-1",
		Bucket:   "test-bucket",
		Key:      "test/key.txt",
	}

	err = store.PutUpload(ctx, upload)
	require.NoError(t, err)
	err = store.PutUpload(ctx, upload)
	require.NoError(t, err)

	stats := store.GetStats()
	assert.Equal(t, int64(1), stats.UploadCount)
}

func TestBoltDBMetadataStore_DeleteMissingUploadDoesNotChangeUploadCount(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	before := store.GetStats()
	err = store.DeleteUpload(ctx, "test-bucket", "test/key.txt", "missing-upload")
	require.NoError(t, err)
	after := store.GetStats()

	assert.Equal(t, before.UploadCount, after.UploadCount)
}

func TestBoltDBMetadataStore_ListExpiredUploads(t *testing.T) {
	store, err := NewBoltDBMetadataStore(t.TempDir() + "/test.db")
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	err = store.PutUpload(ctx, &MultipartUpload{
		UploadID:  "expired-upload",
		Bucket:    "test-bucket",
		Key:       "expired.txt",
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)
	err = store.PutUpload(ctx, &MultipartUpload{
		UploadID:  "active-upload",
		Bucket:    "test-bucket",
		Key:       "active.txt",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	active, err := store.ListUploads(ctx, "test-bucket")
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "active-upload", active[0].UploadID)

	expired, err := store.ListExpiredUploads(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, "expired-upload", expired[0].UploadID)
}

func TestBoltDBMetadataStore_Stats(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	bucket := &BucketInfo{Name: "test-bucket", OwnerID: "user-001"}
	err = store.CreateBucket(ctx, "test-bucket", bucket)
	require.NoError(t, err)

	stats := store.GetStats()
	assert.NotNil(t, stats)
	assert.GreaterOrEqual(t, stats.BucketCount, int64(1))
}

func TestBoltDBMetadataStore_ObjectNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	_, err = store.GetObject(ctx, "nonexistent-bucket", "nonexistent-key")
	assert.Error(t, err)
}

func TestBoltDBMetadataStore_BucketNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	_, err = store.GetBucket(ctx, "nonexistent-bucket")
	assert.Error(t, err)
}

func TestComputeChecksum(t *testing.T) {
	data := []byte("hello world")

	assert.Equal(t, "c99465aa", ComputeChecksum(data, "CRC32C"))
	assert.Equal(t, "0d4a1185", ComputeChecksum(data, "CRC32"))
	assert.Equal(t, "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", ComputeChecksum(data, "SHA256"))
}

func BenchmarkBoltDBMetadataStore_PutObject(b *testing.B) {
	tmpDir := b.TempDir()
	dbPath := tmpDir + "/bench.db"

	store, err := NewBoltDBMetadataStore(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	store.CreateBucket(ctx, "bench-bucket", &BucketInfo{Name: "bench-bucket", OwnerID: "user-001"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		obj := &ObjectMetadata{
			Key:    "bench/key" + string(rune(i)),
			Bucket: "bench-bucket",
			Size:   1024,
		}
		store.PutObject(ctx, "bench-bucket", obj.Key, obj)
	}
}
