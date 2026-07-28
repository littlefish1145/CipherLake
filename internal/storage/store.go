package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"cipherlake/internal/common"
	"cipherlake/internal/config"
	"cipherlake/internal/metadata"
)

var (
	ErrObjectNotFound    = errors.New("object not found")
	ErrBucketNotFound    = errors.New("bucket not found")
	ErrInvalidTier       = errors.New("invalid storage tier")
	ErrTierNotAvailable  = errors.New("storage tier not available")
	ErrInvalidObjectKey  = errors.New("invalid object key")
	ErrInvalidBucketName = errors.New("invalid bucket name")
)

type ObjectStore interface {
	Put(ctx context.Context, bucket, key string, data io.Reader, size int64, metadata *common.ObjectMetadata) error
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, *common.ObjectMetadata, error)
	Delete(ctx context.Context, bucket, key string) error
	Head(ctx context.Context, bucket, key string) (*common.ObjectMetadata, error)
	List(ctx context.Context, bucket, prefix string, maxKeys int) ([]*common.ObjectMetadata, error)
	Copy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error
}

type TieredStore interface {
	Put(ctx context.Context, bucket, key string, data io.Reader, size int64, tier common.StorageTier, metadata *common.ObjectMetadata) error
	Get(ctx context.Context, bucket, key string, tier common.StorageTier) (io.ReadCloser, *common.ObjectMetadata, error)
	Delete(ctx context.Context, bucket, key string, tier common.StorageTier) error
	Head(ctx context.Context, bucket, key string) (*common.ObjectMetadata, error)
	Migrate(ctx context.Context, bucket, key string, fromTier, toTier common.StorageTier) error
	GetTierSize(tier common.StorageTier) (int64, error)
}

type BackendStorage interface {
	io.Closer
	Name() string
	Put(ctx context.Context, path string, data io.Reader, size int64) error
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	Delete(ctx context.Context, path string) error
	Exists(ctx context.Context, path string) (bool, error)
	Size(ctx context.Context, path string) (int64, error)
	List(ctx context.Context, prefix string) ([]string, error)
	PutReader(ctx context.Context, path string, reader io.Reader) (etag string, err error)
	GetRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error)
	AtomicRename(ctx context.Context, oldPath, newPath string) error
}

type FileBackend struct {
	rootDir string
}

func NewFileBackend(rootDir string) (*FileBackend, error) {
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}
	return &FileBackend{
		rootDir: rootDir,
	}, nil
}

func (f *FileBackend) Name() string {
	return "file"
}

func (f *FileBackend) Put(ctx context.Context, path string, data io.Reader, size int64) error {
	fullPath := filepath.Join(f.rootDir, path)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	file, err := os.CreateTemp(filepath.Dir(fullPath), ".tmp-"+filepath.Base(fullPath))
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := file.Name()

	written, err := io.Copy(file, data)
	if err != nil {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Only validate size if it was specified (size > 0)
	// size == 0 means the size is unknown (e.g., encrypted data with GCM overhead)
	if size > 0 && written != size {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("incomplete write: expected %d, got %d", size, written)
	}

	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to sync file: %w", err)
	}

	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

func (f *FileBackend) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	fullPath := filepath.Join(f.rootDir, path)

	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	return file, nil
}

func (f *FileBackend) Delete(ctx context.Context, path string) error {
	fullPath := filepath.Join(f.rootDir, path)

	err := os.Remove(fullPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

func (f *FileBackend) Exists(ctx context.Context, path string) (bool, error) {
	fullPath := filepath.Join(f.rootDir, path)

	_, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat file: %w", err)
	}

	return true, nil
}

func (f *FileBackend) Size(ctx context.Context, path string) (int64, error) {
	fullPath := filepath.Join(f.rootDir, path)

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrObjectNotFound
		}
		return 0, fmt.Errorf("failed to stat file: %w", err)
	}

	return info.Size(), nil
}

func (f *FileBackend) List(ctx context.Context, prefix string) ([]string, error) {
	files := make([]string, 0)
	searchPath := filepath.Join(f.rootDir, prefix)

	if _, err := os.Stat(searchPath); err != nil {
		if os.IsNotExist(err) {
			return files, nil
		}
		return nil, fmt.Errorf("failed to stat list prefix: %w", err)
	}

	err := filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			relPath, err := filepath.Rel(f.rootDir, path)
			if err == nil {
				files = append(files, relPath)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	return files, nil
}

func (f *FileBackend) Close() error {
	return nil
}

func (f *FileBackend) PutReader(ctx context.Context, path string, reader io.Reader) (string, error) {
	fullPath := filepath.Join(f.rootDir, path)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create directory: %w", err)
	}

	// Write to temp file first for atomic semantics
	tmpFile, err := os.CreateTemp(filepath.Dir(fullPath), ".tmp-"+filepath.Base(fullPath))
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	written, err := io.Copy(tmpFile, reader)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to sync temp file: %w", err)
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to rename temp file: %w", err)
	}

	etag := fmt.Sprintf(`"%x"`, written)
	return etag, nil
}

func (f *FileBackend) GetRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	fullPath := filepath.Join(f.rootDir, path)

	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to seek file: %w", err)
	}

	return &limitedReadCloser{Reader: io.LimitReader(file, length), closer: file}, nil
}

func (f *FileBackend) AtomicRename(ctx context.Context, oldPath, newPath string) error {
	fullOldPath := filepath.Join(f.rootDir, oldPath)
	fullNewPath := filepath.Join(f.rootDir, newPath)

	if err := os.MkdirAll(filepath.Dir(fullNewPath), 0755); err != nil {
		return fmt.Errorf("failed to create target directory: %w", err)
	}

	if err := os.Rename(fullOldPath, fullNewPath); err != nil {
		return fmt.Errorf("failed to rename file: %w", err)
	}

	return nil
}

type limitedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (l *limitedReadCloser) Close() error {
	return l.closer.Close()
}

type objectLock struct {
	mu   sync.Mutex
	refs int
}

type TieredObjectStore struct {
	mu          sync.RWMutex
	tiers       map[common.StorageTier]BackendStorage
	currentSize map[common.StorageTier]int64
	maxSize     map[common.StorageTier]int64
	metadata    metadata.MetadataStore
	objLocks    sync.Map
	locksMu     sync.Mutex
}

func NewTieredObjectStore(meta metadata.MetadataStore) *TieredObjectStore {
	return &TieredObjectStore{
		tiers:       make(map[common.StorageTier]BackendStorage),
		currentSize: make(map[common.StorageTier]int64),
		maxSize:     make(map[common.StorageTier]int64),
		metadata:    meta,
	}
}

func (s *TieredObjectStore) RegisterTier(tier common.StorageTier, backend BackendStorage, maxSize int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tiers[tier] = backend
	s.maxSize[tier] = maxSize
}

// RegisterTierFromConfig creates a backend from the storage class config
// and registers it for the given tier.
func (s *TieredObjectStore) RegisterTierFromConfig(tier common.StorageTier, cfg config.StorageClassConfig, maxSize int64) error {
	backend, err := NewBackendFromConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create backend for tier %s: %w", tier.String(), err)
	}
	s.RegisterTier(tier, backend, maxSize)
	return nil
}

// GetTierBackend returns the backend for a given tier.
func (s *TieredObjectStore) GetTierBackend(tier common.StorageTier) (BackendStorage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.tiers[tier]
	return b, ok
}

func (s *TieredObjectStore) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, tier common.StorageTier, metadata *common.ObjectMetadata) error {
	s.lockObject(bucket, key)
	defer s.unlockObject(bucket, key)

	s.mu.RLock()
	path := s.makePath(bucket, key)
	backend, ok := s.tiers[tier]
	s.mu.RUnlock()

	if !ok {
		return ErrTierNotAvailable
	}

	if err := backend.Put(ctx, path, data, size); err != nil {
		return fmt.Errorf("failed to put object: %w", err)
	}

	s.mu.Lock()
	s.currentSize[tier] += size
	s.mu.Unlock()

	return nil
}

func (s *TieredObjectStore) Get(ctx context.Context, bucket, key string, tier common.StorageTier) (io.ReadCloser, *common.ObjectMetadata, error) {
	s.mu.RLock()
	path := s.makePath(bucket, key)
	backend, ok := s.tiers[tier]
	s.mu.RUnlock()

	if !ok {
		return nil, nil, ErrTierNotAvailable
	}

	reader, err := backend.Get(ctx, path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get object: %w", err)
	}

	return reader, nil, nil
}

func (s *TieredObjectStore) Delete(ctx context.Context, bucket, key string, tier common.StorageTier) error {
	s.lockObject(bucket, key)
	defer s.unlockObject(bucket, key)

	s.mu.RLock()
	path := s.makePath(bucket, key)
	backend, ok := s.tiers[tier]
	s.mu.RUnlock()

	if !ok {
		return ErrTierNotAvailable
	}

	size, _ := backend.Size(ctx, path)

	if err := backend.Delete(ctx, path); err != nil {
		return fmt.Errorf("failed to delete object: %w", err)
	}

	s.mu.Lock()
	s.currentSize[tier] -= size
	s.mu.Unlock()

	return nil
}

func (s *TieredObjectStore) Head(ctx context.Context, bucket, key string) (*common.ObjectMetadata, error) {
	if s.metadata == nil {
		return nil, ErrObjectNotFound
	}

	meta, err := s.metadata.GetObject(ctx, bucket, key)
	if err != nil {
		return nil, ErrObjectNotFound
	}

	return metadataToCommon(meta), nil
}

func (s *TieredObjectStore) Migrate(ctx context.Context, bucket, key string, fromTier, toTier common.StorageTier) error {
	s.lockObject(bucket, key)
	defer s.unlockObject(bucket, key)

	if s.metadata == nil {
		return ErrObjectNotFound
	}

	meta, err := s.metadata.GetObject(ctx, bucket, key)
	if err != nil {
		return ErrObjectNotFound
	}

	if common.StorageTier(meta.StorageTier) != fromTier {
		return fmt.Errorf("object is not in source tier")
	}

	s.mu.RLock()
	srcPath := s.makePath(bucket, key)
	srcBackend, ok := s.tiers[fromTier]
	dstBackend, ok2 := s.tiers[toTier]
	s.mu.RUnlock()

	if !ok || !ok2 {
		return ErrTierNotAvailable
	}

	reader, err := srcBackend.Get(ctx, srcPath)
	if err != nil {
		return fmt.Errorf("failed to read from source: %w", err)
	}

	if err := dstBackend.Put(ctx, srcPath, reader, meta.Size); err != nil {
		reader.Close()
		return fmt.Errorf("failed to write to destination: %w", err)
	}
	if err := reader.Close(); err != nil {
		return fmt.Errorf("failed to close source after migration: %w", err)
	}

	if err := srcBackend.Delete(ctx, srcPath); err != nil {
		return fmt.Errorf("failed to delete source after migration: %w", err)
	}

	s.mu.Lock()
	s.currentSize[fromTier] -= meta.Size
	s.currentSize[toTier] += meta.Size
	s.mu.Unlock()

	meta.StorageTier = int(toTier)
	if err := s.metadata.PutObject(ctx, bucket, key, meta); err != nil {
		return fmt.Errorf("failed to update metadata after migration: %w", err)
	}

	return nil
}

func (s *TieredObjectStore) GetTierSize(tier common.StorageTier) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentSize[tier], nil
}

func (s *TieredObjectStore) lockObject(bucket, key string) {
	objKey := bucket + "/" + key

	actual, _ := s.objLocks.LoadOrStore(objKey, &objectLock{})
	ol := actual.(*objectLock)

	s.locksMu.Lock()
	ol.refs++
	s.locksMu.Unlock()

	ol.mu.Lock()
}

func (s *TieredObjectStore) unlockObject(bucket, key string) {
	objKey := bucket + "/" + key

	actual, ok := s.objLocks.Load(objKey)
	if !ok {
		return
	}
	ol := actual.(*objectLock)

	ol.mu.Unlock()

	s.locksMu.Lock()
	ol.refs--
	if ol.refs == 0 {
		s.objLocks.Delete(objKey)
	}
	s.locksMu.Unlock()
}

func (s *TieredObjectStore) makePath(bucket, key string) string {
	return bucket + "/" + key
}

// metadataToCommon converts a metadata.ObjectMetadata into the common
// ObjectMetadata shape used by the storage layer and tiering manager.
func metadataToCommon(m *metadata.ObjectMetadata) *common.ObjectMetadata {
	if m == nil {
		return nil
	}
	return &common.ObjectMetadata{
		Key:             m.Key,
		Bucket:          m.Bucket,
		Size:            m.Size,
		ContentType:     m.ContentType,
		ContentEncoding: m.ContentEncoding,
		ETag:            m.ETag,
		UserMetadata:    m.UserMetadata,
		StorageTier:     common.StorageTier(m.StorageTier),
		CreatedAt:       m.CreatedAt,
		ModifiedAt:      m.ModifiedAt,
		AccessCount:     m.AccessCount,
		LastAccessedAt:  m.LastAccessedAt,
		Encrypted:       m.Encrypted,
		Vectorized:      m.Vectorized,
		VersionID:       m.VersionID,
	}
}
