package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"nexus/internal/common"
	"nexus/internal/metadata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type memMetadataStore struct {
	objects map[string]*metadata.ObjectMetadata
}

func newMemMetadataStore() *memMetadataStore {
	return &memMetadataStore{objects: make(map[string]*metadata.ObjectMetadata)}
}

func (m *memMetadataStore) key(bucket, key string) string { return bucket + "/" + key }

func (m *memMetadataStore) PutObject(ctx context.Context, bucket, key string, meta *metadata.ObjectMetadata) error {
	m.objects[m.key(bucket, key)] = meta
	return nil
}

func (m *memMetadataStore) GetObject(ctx context.Context, bucket, key string) (*metadata.ObjectMetadata, error) {
	meta, ok := m.objects[m.key(bucket, key)]
	if !ok {
		return nil, errors.New("not found")
	}
	return meta, nil
}

func (m *memMetadataStore) DeleteObject(ctx context.Context, bucket, key string) error {
	delete(m.objects, m.key(bucket, key))
	return nil
}

func (m *memMetadataStore) ListObjects(ctx context.Context, bucket, prefix string, maxKeys int) ([]*metadata.ObjectMetadata, error) {
	return nil, nil
}

func (m *memMetadataStore) ListObjectsWithDelimiter(ctx context.Context, bucket, prefix, delimiter string, maxKeys int) (*metadata.ListResult, error) {
	return nil, nil
}

func (m *memMetadataStore) CreateBucket(ctx context.Context, bucket string, info *metadata.BucketInfo) error {
	return nil
}

func (m *memMetadataStore) GetBucket(ctx context.Context, bucket string) (*metadata.BucketInfo, error) {
	return nil, nil
}

func (m *memMetadataStore) DeleteBucket(ctx context.Context, bucket string) error {
	return nil
}

func (m *memMetadataStore) ListBuckets(ctx context.Context) ([]*metadata.BucketInfo, error) {
	return nil, nil
}

func (m *memMetadataStore) UpdateBucket(ctx context.Context, bucket string, info *metadata.BucketInfo) error {
	return nil
}

func (m *memMetadataStore) PutObjectVersion(ctx context.Context, bucket, key string, meta *metadata.ObjectMetadata) error {
	return nil
}

func (m *memMetadataStore) GetObjectVersion(ctx context.Context, bucket, key, versionID string) (*metadata.ObjectMetadata, error) {
	return nil, nil
}

func (m *memMetadataStore) ListObjectVersions(ctx context.Context, bucket, key string, maxVersions int) ([]*metadata.ObjectMetadata, error) {
	return nil, nil
}

func (m *memMetadataStore) PutUpload(ctx context.Context, upload *metadata.MultipartUpload) error {
	return nil
}

func (m *memMetadataStore) GetUpload(ctx context.Context, bucket, key, uploadID string) (*metadata.MultipartUpload, error) {
	return nil, nil
}

func (m *memMetadataStore) DeleteUpload(ctx context.Context, bucket, key, uploadID string) error {
	return nil
}

func (m *memMetadataStore) ListUploads(ctx context.Context, bucket string) ([]*metadata.MultipartUpload, error) {
	return nil, nil
}

func (m *memMetadataStore) AddPart(ctx context.Context, uploadID string, part *metadata.UploadPart) error {
	return nil
}

func (m *memMetadataStore) GetParts(ctx context.Context, uploadID string) ([]*metadata.UploadPart, error) {
	return nil, nil
}

func (m *memMetadataStore) Close() error                      { return nil }
func (m *memMetadataStore) FlushWAL() error                   { return nil }
func (m *memMetadataStore) GetStats() *metadata.MetadataStats { return nil }

func newTestStore(t *testing.T) (*TieredObjectStore, *memMetadataStore) {
	t.Helper()
	meta := newMemMetadataStore()
	store := NewTieredObjectStore(meta)
	return store, meta
}

func registerHot(t *testing.T, store *TieredObjectStore) {
	t.Helper()
	backend, err := NewFileBackend(t.TempDir())
	require.NoError(t, err)
	store.RegisterTier(common.TierHot, backend, 1<<30)
}

func TestTieredObjectStore_PutAndGet(t *testing.T) {
	store, _ := newTestStore(t)
	registerHot(t, store)
	ctx := context.Background()

	data := []byte("hello world")
	err := store.Put(ctx, "bucket", "key", bytes.NewReader(data), int64(len(data)), common.TierHot, nil)
	require.NoError(t, err)

	reader, meta, err := store.Get(ctx, "bucket", "key", common.TierHot)
	require.NoError(t, err)
	defer reader.Close()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Nil(t, meta)
}

func TestTieredObjectStore_Delete(t *testing.T) {
	store, _ := newTestStore(t)
	registerHot(t, store)
	ctx := context.Background()

	data := []byte("hello world")
	err := store.Put(ctx, "bucket", "key", bytes.NewReader(data), int64(len(data)), common.TierHot, nil)
	require.NoError(t, err)

	size, err := store.GetTierSize(common.TierHot)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)

	err = store.Delete(ctx, "bucket", "key", common.TierHot)
	require.NoError(t, err)

	size, err = store.GetTierSize(common.TierHot)
	require.NoError(t, err)
	assert.Equal(t, int64(0), size)
}

func TestTieredObjectStore_Head(t *testing.T) {
	store, meta := newTestStore(t)
	ctx := context.Background()

	meta.objects["bucket/key"] = &metadata.ObjectMetadata{
		Key:         "key",
		Bucket:      "bucket",
		Size:        42,
		StorageTier: int(common.TierHot),
		ContentType: "text/plain",
		ModifiedAt:  time.Now(),
	}

	got, err := store.Head(ctx, "bucket", "key")
	require.NoError(t, err)
	assert.Equal(t, "key", got.Key)
	assert.Equal(t, int64(42), got.Size)
	assert.Equal(t, common.TierHot, got.StorageTier)
}

func TestTieredObjectStore_HeadNotFound(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	_, err := store.Head(ctx, "bucket", "missing")
	assert.ErrorIs(t, err, ErrObjectNotFound)
}

func TestTieredObjectStore_Migrate(t *testing.T) {
	store, meta := newTestStore(t)
	ctx := context.Background()

	hotBackend, err := NewFileBackend(t.TempDir())
	require.NoError(t, err)
	store.RegisterTier(common.TierHot, hotBackend, 1<<30)

	warmBackend, err := NewFileBackend(t.TempDir())
	require.NoError(t, err)
	store.RegisterTier(common.TierWarm, warmBackend, 5<<30)

	data := []byte("migrate me")
	err = store.Put(ctx, "bucket", "key", bytes.NewReader(data), int64(len(data)), common.TierHot, nil)
	require.NoError(t, err)

	meta.objects["bucket/key"] = &metadata.ObjectMetadata{
		Key:         "key",
		Bucket:      "bucket",
		Size:        int64(len(data)),
		StorageTier: int(common.TierHot),
	}

	err = store.Migrate(ctx, "bucket", "key", common.TierHot, common.TierWarm)
	require.NoError(t, err)

	updated := meta.objects["bucket/key"]
	require.NotNil(t, updated)
	assert.Equal(t, int(common.TierWarm), updated.StorageTier)

	exists, err := warmBackend.Exists(ctx, "bucket/key")
	require.NoError(t, err)
	assert.True(t, exists)
}
