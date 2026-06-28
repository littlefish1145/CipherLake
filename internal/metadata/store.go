package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrKeyNotFound        = errors.New("key not found")
	ErrBucketNotFound     = errors.New("bucket not found")
	ErrInvalidKey        = errors.New("invalid key")
	ErrTransactionFailed = errors.New("transaction failed")
	ErrQuotaExceeded     = errors.New("quota exceeded")
	ErrVersionNotFound   = errors.New("version not found")
)

type MetadataStore interface {
	PutObject(ctx context.Context, bucket, key string, metadata *ObjectMetadata) error
	GetObject(ctx context.Context, bucket, key string) (*ObjectMetadata, error)
	DeleteObject(ctx context.Context, bucket, key string) error
	ListObjects(ctx context.Context, bucket, prefix string, maxKeys int) ([]*ObjectMetadata, error)
	ListObjectsWithDelimiter(ctx context.Context, bucket, prefix, delimiter string, maxKeys int) (*ListResult, error)
	CreateBucket(ctx context.Context, bucket string, info *BucketInfo) error
	GetBucket(ctx context.Context, bucket string) (*BucketInfo, error)
	DeleteBucket(ctx context.Context, bucket string) error
	ListBuckets(ctx context.Context) ([]*BucketInfo, error)
	UpdateBucket(ctx context.Context, bucket string, info *BucketInfo) error
	PutObjectVersion(ctx context.Context, bucket, key string, metadata *ObjectMetadata) error
	GetObjectVersion(ctx context.Context, bucket, key, versionID string) (*ObjectMetadata, error)
	ListObjectVersions(ctx context.Context, bucket, key string, maxVersions int) ([]*ObjectMetadata, error)
	PutUpload(ctx context.Context, upload *MultipartUpload) error
	GetUpload(ctx context.Context, bucket, key, uploadID string) (*MultipartUpload, error)
	DeleteUpload(ctx context.Context, bucket, key, uploadID string) error
	ListUploads(ctx context.Context, bucket string) ([]*MultipartUpload, error)
	AddPart(ctx context.Context, uploadID string, part *UploadPart) error
	GetParts(ctx context.Context, uploadID string) ([]*UploadPart, error)
	Close() error
	FlushWAL() error
	GetStats() *MetadataStats
}

type ListResult struct {
	Objects        []*ObjectMetadata
	CommonPrefixes []string
	IsTruncated    bool
	NextMarker     string
}

type ObjectMetadata struct {
	Key              string            `json:"key"`
	Bucket           string            `json:"bucket"`
	Size             int64             `json:"size"`
	ContentType      string            `json:"content_type"`
	ContentEncoding  string            `json:"content_encoding"`
	ETag             string            `json:"etag"`
	Checksum         string            `json:"checksum"`
	ChecksumType     string            `json:"checksum_type"`
	UserMetadata     map[string]string `json:"user_metadata"`
	StorageTier      int               `json:"storage_tier"`
	CreatedAt        time.Time         `json:"created_at"`
	ModifiedAt       time.Time         `json:"modified_at"`
	AccessCount      int64             `json:"access_count"`
	LastAccessedAt   time.Time         `json:"last_accessed_at"`
	Encrypted        bool              `json:"encrypted"`
	EncryptedDEK     []byte            `json:"encrypted_dek,omitempty"`
	Vectorized       bool              `json:"vectorized"`
	VectorID         string            `json:"vector_id,omitempty"`
	VersionID        string            `json:"version_id"`
	IsLatest         bool              `json:"is_latest"`
	PreviousVersion  string            `json:"previous_version,omitempty"`
	ObjectStatus     string            `json:"object_status"`
	TieringLocked    bool              `json:"tiering_locked"`
	LockedTier       int               `json:"locked_tier,omitempty"`
	PipelineResults  map[string]string `json:"pipeline_results,omitempty"`
	DeleteMarker     bool              `json:"delete_marker,omitempty"`
	RetainUntil      *time.Time        `json:"retain_until,omitempty"`
	SSECUsed         bool              `json:"ssec_used"`
	SSECKeySHA256    string            `json:"ssec_key_sha256"`
	SSECAlgorithm    string            `json:"ssec_algorithm"`
}

type BucketInfo struct {
	Name           string                `json:"name"`
	CreatedAt      time.Time             `json:"created_at"`
	OwnerID        string                `json:"owner_id"`
	OwnerName      string                `json:"owner_name"`
	Region         string                `json:"region"`
	ACL            string                `json:"acl,omitempty"`
	Quota          *QuotaConfig          `json:"quota,omitempty"`
	Policy         map[string]any        `json:"policy,omitempty"`
	Tags           map[string]string     `json:"tags,omitempty"`
	CORS           *CORSConfiguration    `json:"cors,omitempty"`
	Lifecycle      *LifecycleRule        `json:"lifecycle,omitempty"`
	Encryption     *BucketEncryption     `json:"encryption,omitempty"`
	ObjectCount    int64                `json:"object_count"`
	TotalSize      int64                `json:"total_size"`
	Versioning     bool                 `json:"versioning"`
	LoggingEnabled bool                 `json:"logging_enabled"`
	ObjectLock     *ObjectLockConfig     `json:"object_lock,omitempty"`
	StorageClass   string               `json:"storage_class"`
}

type QuotaConfig struct {
	MaxObjects   int64 `json:"max_objects"`
	MaxSizeBytes int64 `json:"max_size_bytes"`
	WarnThreshold float64 `json:"warn_threshold"`
}

type CORSConfiguration struct {
	AllowedOrigins []string `json:"allowed_origins"`
	AllowedMethods []string `json:"allowed_methods"`
	AllowedHeaders []string `json:"allowed_headers"`
	MaxAgeSeconds  int      `json:"max_age_seconds"`
}

type LifecycleRule struct {
	Enabled    bool            `json:"enabled"`
	Prefix     string          `json:"prefix"`
	Expiration *ExpirationRule `json:"expiration,omitempty"`
	Transitions []TransitionRule `json:"transitions,omitempty"`
}

type ExpirationRule struct {
	Days              int       `json:"days"`
	Date             *time.Time `json:"date,omitempty"`
	ExpiredObjectDeleteMarker bool `json:"expired_object_delete_marker"`
}

type TransitionRule struct {
	Days         int   `json:"days"`
	StorageClass int   `json:"storage_class"`
}

type BucketEncryption struct {
	Algorithm   string `json:"algorithm"`
	KMSKeyID    string `json:"kms_key_id,omitempty"`
}

type ObjectLockConfig struct {
	Enabled  bool   `json:"enabled"`
	Mode     string `json:"mode"`
	Days     int    `json:"days"`
}

type MultipartUpload struct {
	UploadID   string    `json:"upload_id"`
	Bucket     string    `json:"bucket"`
	Key        string    `json:"key"`
	Initiated  time.Time `json:"initiated"`
	ExpiresAt  time.Time `json:"expires_at"`
	UserID     string    `json:"user_id"`
	ContentType string   `json:"content_type"`
	Metadata   map[string]string `json:"metadata"`
	Parts      []*UploadPart `json:"parts,omitempty"`
	TotalSize  int64   `json:"total_size"`
	Encrypted  bool    `json:"encrypted"`
	Checksum   string  `json:"checksum"`
}

type ResumableSession struct {
	UploadID    string            `json:"upload_id"`
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	Offset      int64             `json:"offset"`
	TotalSize   int64             `json:"total_size"`
	ContentType string            `json:"content_type"`
	Metadata    map[string]string `json:"metadata"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Checksum    string            `json:"checksum"`
	Finalized   bool              `json:"finalized"`
}

type UploadPart struct {
	PartNumber   int       `json:"part_number"`
	ETag         string    `json:"etag"`
	Size         int64     `json:"size"`
	Checksum     string    `json:"checksum"`
	UploadedAt   time.Time `json:"uploaded_at"`
	ChecksumType string    `json:"checksum_type"`
}

type WALEntry struct {
	Type      string          `json:"type"`
	Bucket    string          `json:"bucket"`
	Key       string          `json:"key,omitempty"`
	Data      json.RawMessage `json:"data"`
	Timestamp time.Time       `json:"timestamp"`
	Checksum  uint32         `json:"checksum"`
}

type BoltDBMetadataStore struct {
	mu       sync.RWMutex
	db       *bolt.DB
	path     string
	walPath  string
	walFile  *os.File
	walBuf   []WALEntry
	walMu    sync.Mutex
	syncedUp int64
	stats    MetadataStats
}

type MetadataStats struct {
	ObjectCount    int64     `json:"object_count"`
	BucketCount    int64     `json:"bucket_count"`
	TotalSize      int64     `json:"total_size"`
	UploadCount    int64     `json:"upload_count"`
	WALEntries     int64     `json:"wal_entries"`
	LastCompaction time.Time `json:"last_compaction"`
}

func NewBoltDBMetadataStore(path string) (*BoltDBMetadataStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("failed to create metadata directory: %w", err)
	}

	db, err := bolt.Open(path, 0666, &bolt.Options{
		Timeout:        5 * time.Second,
		FreelistType:   bolt.FreelistMapType,
		MmapFlags:      os.O_RDWR,
		NoSync:         false,
		NoGrowSync:     false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open bolt db: %w", err)
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		buckets := []string{
			"buckets",
			"objects",
			"object_versions",
			"uploads",
			"upload_parts",
			"access_history",
			"vectors",
			"pipelines",
			"resumable_uploads",
		}
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", name, err)
			}
		}

		if _, err := tx.CreateBucketIfNotExists([]byte("bucket_index")); err != nil {
			return fmt.Errorf("failed to create bucket_index: %w", err)
		}
		if _, err := tx.CreateBucketIfNotExists([]byte("object_index")); err != nil {
			return fmt.Errorf("failed to create object_index: %w", err)
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize buckets: %w", err)
	}

	walPath := path + ".wal"
	store := &BoltDBMetadataStore{
		db:      db,
		path:    path,
		walPath: walPath,
		walBuf:  make([]WALEntry, 0, 1000),
	}

	return store, nil
}

func (s *BoltDBMetadataStore) writeWAL(entry WALEntry) error {
	s.walMu.Lock()
	defer s.walMu.Unlock()

	entry.Checksum = crc32.ChecksumIEEE(entry.Data)
	s.walBuf = append(s.walBuf, entry)

	if len(s.walBuf) >= 100 {
		return s.flushWAL()
	}
	return nil
}

func (s *BoltDBMetadataStore) flushWAL() error {
	if len(s.walBuf) == 0 {
		return nil
	}

	data, err := json.Marshal(s.walBuf)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(s.walPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}

	s.walBuf = s.walBuf[:0]
	atomic.AddInt64(&s.syncedUp, 1)
	return nil
}

func (s *BoltDBMetadataStore) FlushWAL() error {
	return s.flushWAL()
}

func (s *BoltDBMetadataStore) objectKey(bucket, key string) []byte {
	return []byte(bucket + "/" + key)
}

func (s *BoltDBMetadataStore) indexKey(bucket, prefix string) []byte {
	return []byte(bucket + "/" + prefix)
}

func (s *BoltDBMetadataStore) PutObject(ctx context.Context, bucket, key string, metadata *ObjectMetadata) error {
	if bucket == "" || key == "" {
		return ErrInvalidKey
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if metadata.VersionID == "" {
		metadata.VersionID = uuid.New().String()
	}
	if metadata.CreatedAt.IsZero() {
		metadata.CreatedAt = time.Now()
	}
	metadata.ModifiedAt = time.Now()
	metadata.IsLatest = true

	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := s.writeWAL(WALEntry{
		Type:   "put_object",
		Bucket: bucket,
		Key:    key,
		Data:   data,
	}); err != nil {
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		if err := objBucket.Put(s.objectKey(bucket, key), data); err != nil {
			return err
		}

		indexBucket := tx.Bucket([]byte("object_index"))
		prefix := extractPrefix(key)
		indexKey := s.indexKey(bucket, prefix)
		existing := indexBucket.Get(indexKey)
		if existing == nil {
			indexBucket.Put(indexKey, data)
		}

		bktBucket := tx.Bucket([]byte("buckets"))
		bucketData := bktBucket.Get([]byte(bucket))
		if bucketData != nil {
			var info BucketInfo
			if err := json.Unmarshal(bucketData, &info); err == nil {
				info.ObjectCount++
				info.TotalSize += metadata.Size
				if info.Quota != nil {
					if info.Quota.MaxObjects > 0 && info.ObjectCount > info.Quota.MaxObjects {
						return ErrQuotaExceeded
					}
					if info.Quota.MaxSizeBytes > 0 && info.TotalSize > info.Quota.MaxSizeBytes {
						return ErrQuotaExceeded
					}
				}
				newData, _ := json.Marshal(&info)
				bktBucket.Put([]byte(bucket), newData)
			}
		}

		atomic.AddInt64(&s.stats.ObjectCount, 1)
		atomic.AddInt64(&s.stats.TotalSize, metadata.Size)
		return nil
	})
}

func extractPrefix(key string) string {
	lastSlash := -1
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			lastSlash = i
			break
		}
	}
	if lastSlash == -1 {
		return ""
	}
	return key[:lastSlash+1]
}

func (s *BoltDBMetadataStore) GetObject(ctx context.Context, bucket, key string) (*ObjectMetadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var metadata *ObjectMetadata
	err := s.db.View(func(tx *bolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		data := objBucket.Get(s.objectKey(bucket, key))
		if data == nil {
			return ErrKeyNotFound
		}
		return json.Unmarshal(data, &metadata)
	})

	if err != nil {
		return nil, err
	}
	return metadata, nil
}

func (s *BoltDBMetadataStore) DeleteObject(ctx context.Context, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		data := objBucket.Get(s.objectKey(bucket, key))

		if data != nil {
			var metadata ObjectMetadata
			if err := json.Unmarshal(data, &metadata); err == nil {
				bktBucket := tx.Bucket([]byte("buckets"))
				bucketData := bktBucket.Get([]byte(bucket))
				if bucketData != nil {
					var info BucketInfo
					if err := json.Unmarshal(bucketData, &info); err == nil {
						info.ObjectCount--
						info.TotalSize -= metadata.Size
						if info.ObjectCount < 0 {
							info.ObjectCount = 0
						}
						if info.TotalSize < 0 {
							info.TotalSize = 0
						}
						newData, _ := json.Marshal(&info)
						bktBucket.Put([]byte(bucket), newData)
					}
				}
			}
		}

		atomic.AddInt64(&s.stats.ObjectCount, -1)
		return objBucket.Delete(s.objectKey(bucket, key))
	})
}

func (s *BoltDBMetadataStore) ListObjects(ctx context.Context, bucket, prefix string, maxKeys int) ([]*ObjectMetadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*ObjectMetadata
	searchPrefix := bucket + "/" + prefix

	err := s.db.View(func(tx *bolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		cursor := objBucket.Cursor()

		var count int
		for k, v := cursor.Seek([]byte(searchPrefix)); k != nil && count < maxKeys; k, v = cursor.Next() {
			key := string(k)
			if len(key) < len(searchPrefix) || key[:len(searchPrefix)] != searchPrefix {
				break
			}

			var metadata ObjectMetadata
			if err := json.Unmarshal(v, &metadata); err == nil && metadata.IsLatest && !metadata.DeleteMarker {
				results = append(results, &metadata)
				count++
			}
		}

		return nil
	})

	return results, err
}

func (s *BoltDBMetadataStore) ListObjectsWithDelimiter(ctx context.Context, bucket, prefix, delimiter string, maxKeys int) (*ListResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := &ListResult{
		Objects:        make([]*ObjectMetadata, 0),
		CommonPrefixes: make([]string, 0),
	}

	searchPrefix := bucket + "/" + prefix
	searchLen := len(searchPrefix)

	err := s.db.View(func(tx *bolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		cursor := objBucket.Cursor()

		seenPrefixes := make(map[string]bool)
		var count int

		for k, v := cursor.Seek([]byte(searchPrefix)); k != nil && count < maxKeys; k, v = cursor.Next() {
			key := string(k)
			if len(key) < searchLen || key[:searchLen] != searchPrefix {
				break
			}

			objectKey := key[searchLen:]
			remainder := objectKey[len(prefix):]

			delimiterIdx := -1
			if delimiter != "" {
				delimiterIdx = indexOf(remainder, delimiter)
			}

			if delimiterIdx >= 0 {
				commonPrefix := prefix + remainder[:delimiterIdx+len(delimiter)]
				if !seenPrefixes[commonPrefix] {
					seenPrefixes[commonPrefix] = true
					result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix)
				}
				continue
			}

			var metadata ObjectMetadata
			if err := json.Unmarshal(v, &metadata); err == nil && metadata.IsLatest && !metadata.DeleteMarker {
				result.Objects = append(result.Objects, &metadata)
				count++
			}
		}

		result.IsTruncated = count == maxKeys
		if result.IsTruncated && len(result.Objects) > 0 {
			result.NextMarker = result.Objects[len(result.Objects)-1].Key
		}

		return nil
	})

	return result, err
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func (s *BoltDBMetadataStore) CreateBucket(ctx context.Context, bucket string, info *BucketInfo) error {
	if bucket == "" {
		return ErrInvalidKey
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if info == nil {
		info = &BucketInfo{
			CreatedAt: time.Now(),
		}
	}
	info.Name = bucket

	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to marshal bucket info: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		bucketBucket := tx.Bucket([]byte("buckets"))
		if bucketBucket.Get([]byte(bucket)) != nil {
			return fmt.Errorf("bucket %s already exists", bucket)
		}
		if err := bucketBucket.Put([]byte(bucket), data); err != nil {
			return err
		}

		atomic.AddInt64(&s.stats.BucketCount, 1)
		return nil
	})
}

func (s *BoltDBMetadataStore) GetBucket(ctx context.Context, bucket string) (*BucketInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var info *BucketInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		bucketBucket := tx.Bucket([]byte("buckets"))
		data := bucketBucket.Get([]byte(bucket))
		if data == nil {
			return ErrBucketNotFound
		}
		return json.Unmarshal(data, &info)
	})

	return info, err
}

func (s *BoltDBMetadataStore) DeleteBucket(ctx context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		prefix := []byte(bucket + "/")
		cursor := objBucket.Cursor()
		var keysToDelete [][]byte
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
			if bytesHasPrefix(k, prefix) {
				keysToDelete = append(keysToDelete, k)
			}
		}
		for _, k := range keysToDelete {
			if err := objBucket.Delete(k); err != nil {
				return err
			}
		}

		bucketBucket := tx.Bucket([]byte("buckets"))
		if err := bucketBucket.Delete([]byte(bucket)); err != nil {
			return err
		}

		atomic.AddInt64(&s.stats.BucketCount, -1)
		return nil
	})
}

func bytesHasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

func (s *BoltDBMetadataStore) ListBuckets(ctx context.Context) ([]*BucketInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*BucketInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		bucketBucket := tx.Bucket([]byte("buckets"))
		return bucketBucket.ForEach(func(k, v []byte) error {
			var info BucketInfo
			if err := json.Unmarshal(v, &info); err == nil {
				results = append(results, &info)
			}
			return nil
		})
	})

	return results, err
}

func (s *BoltDBMetadataStore) UpdateBucket(ctx context.Context, bucket string, info *BucketInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to marshal bucket info: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		bucketBucket := tx.Bucket([]byte("buckets"))
		return bucketBucket.Put([]byte(bucket), data)
	})
}

func (s *BoltDBMetadataStore) PutObjectVersion(ctx context.Context, bucket, key string, metadata *ObjectMetadata) error {
	if metadata.VersionID == "" {
		metadata.VersionID = uuid.New().String()
	}
	if metadata.CreatedAt.IsZero() {
		metadata.CreatedAt = time.Now()
	}
	metadata.ModifiedAt = time.Now()

	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		verBucket := tx.Bucket([]byte("object_versions"))
		versionKey := []byte(fmt.Sprintf("%s/%s/%s", bucket, key, metadata.VersionID))
		return verBucket.Put(versionKey, data)
	})
}

func (s *BoltDBMetadataStore) GetObjectVersion(ctx context.Context, bucket, key, versionID string) (*ObjectMetadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var metadata *ObjectMetadata
	err := s.db.View(func(tx *bolt.Tx) error {
		verBucket := tx.Bucket([]byte("object_versions"))
		versionKey := []byte(fmt.Sprintf("%s/%s/%s", bucket, key, versionID))
		data := verBucket.Get(versionKey)
		if data == nil {
			return ErrVersionNotFound
		}
		return json.Unmarshal(data, &metadata)
	})

	return metadata, err
}

func (s *BoltDBMetadataStore) ListObjectVersions(ctx context.Context, bucket, key string, maxVersions int) ([]*ObjectMetadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*ObjectMetadata
	prefix := fmt.Sprintf("%s/%s/", bucket, key)

	err := s.db.View(func(tx *bolt.Tx) error {
		verBucket := tx.Bucket([]byte("object_versions"))
		cursor := verBucket.Cursor()

		var count int
		for k, v := cursor.Seek([]byte(prefix)); k != nil && count < maxVersions; k, v = cursor.Next() {
			keyStr := string(k)
			if len(keyStr) < len(prefix) || keyStr[:len(prefix)] != prefix {
				break
			}

			var metadata ObjectMetadata
			if err := json.Unmarshal(v, &metadata); err == nil {
				results = append(results, &metadata)
				count++
			}
		}

		return nil
	})

	return results, err
}

func (s *BoltDBMetadataStore) PutUpload(ctx context.Context, upload *MultipartUpload) error {
	if upload.UploadID == "" {
		upload.UploadID = uuid.New().String()
	}
	upload.Initiated = time.Now()
	upload.ExpiresAt = time.Now().Add(24 * time.Hour)

	data, err := json.Marshal(upload)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		uploadBucket := tx.Bucket([]byte("uploads"))
		uploadKey := []byte(fmt.Sprintf("%s/%s/%s", upload.Bucket, upload.Key, upload.UploadID))
		if err := uploadBucket.Put(uploadKey, data); err != nil {
			return err
		}

		atomic.AddInt64(&s.stats.UploadCount, 1)
		return nil
	})
}

func (s *BoltDBMetadataStore) GetUpload(ctx context.Context, bucket, key, uploadID string) (*MultipartUpload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var upload *MultipartUpload
	err := s.db.View(func(tx *bolt.Tx) error {
		uploadBucket := tx.Bucket([]byte("uploads"))
		uploadKey := []byte(fmt.Sprintf("%s/%s/%s", bucket, key, uploadID))
		data := uploadBucket.Get(uploadKey)
		if data == nil {
			return ErrKeyNotFound
		}
		return json.Unmarshal(data, &upload)
	})

	return upload, err
}

func (s *BoltDBMetadataStore) DeleteUpload(ctx context.Context, bucket, key, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		uploadBucket := tx.Bucket([]byte("uploads"))
		uploadKey := []byte(fmt.Sprintf("%s/%s/%s", bucket, key, uploadID))
		if err := uploadBucket.Delete(uploadKey); err != nil {
			return err
		}

		partsBucket := tx.Bucket([]byte("upload_parts"))
		partsPrefix := uploadID + "/"
		partsCursor := partsBucket.Cursor()
		var partsToDelete [][]byte
		for k, _ := partsCursor.Seek([]byte(partsPrefix)); k != nil; k, _ = partsCursor.Next() {
			keyStr := string(k)
			if len(keyStr) < len(partsPrefix) || keyStr[:len(partsPrefix)] != partsPrefix {
				break
			}
			partsToDelete = append(partsToDelete, k)
		}
		for _, k := range partsToDelete {
			partsBucket.Delete(k)
		}

		atomic.AddInt64(&s.stats.UploadCount, -1)
		return nil
	})
}

func (s *BoltDBMetadataStore) ListUploads(ctx context.Context, bucket string) ([]*MultipartUpload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*MultipartUpload
	prefix := bucket + "/"

	err := s.db.View(func(tx *bolt.Tx) error {
		uploadBucket := tx.Bucket([]byte("uploads"))
		cursor := uploadBucket.Cursor()

		now := time.Now()
		for k, v := cursor.Seek([]byte(prefix)); k != nil; k, v = cursor.Next() {
			keyStr := string(k)
			if len(keyStr) < len(prefix) || keyStr[:len(prefix)] != prefix {
				break
			}

			var upload MultipartUpload
			if err := json.Unmarshal(v, &upload); err == nil {
				if upload.ExpiresAt.After(now) {
					results = append(results, &upload)
				}
			}
		}

		return nil
	})

	return results, err
}

func (s *BoltDBMetadataStore) AddPart(ctx context.Context, uploadID string, part *UploadPart) error {
	data, err := json.Marshal(part)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		partsBucket := tx.Bucket([]byte("upload_parts"))
		partKey := []byte(fmt.Sprintf("%s/%d", uploadID, part.PartNumber))
		return partsBucket.Put(partKey, data)
	})
}

func (s *BoltDBMetadataStore) GetParts(ctx context.Context, uploadID string) ([]*UploadPart, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var parts []*UploadPart

	err := s.db.View(func(tx *bolt.Tx) error {
		partsBucket := tx.Bucket([]byte("upload_parts"))
		partsPrefix := uploadID + "/"
		cursor := partsBucket.Cursor()

		for k, v := cursor.Seek([]byte(partsPrefix)); k != nil; k, v = cursor.Next() {
			keyStr := string(k)
			if len(keyStr) < len(partsPrefix) || keyStr[:len(partsPrefix)] != partsPrefix {
				break
			}

			var part UploadPart
			if err := json.Unmarshal(v, &part); err == nil {
				parts = append(parts, &part)
			}
		}

		return nil
	})

	return parts, err
}

func (s *BoltDBMetadataStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.flushWAL(); err != nil {
	}
	return s.db.Close()
}

func (s *BoltDBMetadataStore) GetStats() *MetadataStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return &MetadataStats{
		ObjectCount:    atomic.LoadInt64(&s.stats.ObjectCount),
		BucketCount:    atomic.LoadInt64(&s.stats.BucketCount),
		TotalSize:      atomic.LoadInt64(&s.stats.TotalSize),
		UploadCount:    atomic.LoadInt64(&s.stats.UploadCount),
		WALEntries:     atomic.LoadInt64(&s.syncedUp),
		LastCompaction: s.stats.LastCompaction,
	}
}

func ComputeChecksum(data []byte, checksumType string) string {
	switch checksumType {
	case "CRC32C":
		return fmt.Sprintf("%08x", crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
	case "SHA256":
		import_crypto_sha256()
		return fmt.Sprintf("%x", data)
	default:
		return fmt.Sprintf("%08x", crc32.ChecksumIEEE(data))
	}
}

func import_crypto_sha256() {}

func (s *BoltDBMetadataStore) PutResumableSession(ctx context.Context, session *ResumableSession) error {
	data, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("failed to marshal resumable session: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("resumable_uploads"))
		return bucket.Put([]byte(session.UploadID), data)
	})
}

func (s *BoltDBMetadataStore) GetResumableSession(ctx context.Context, uploadID string) (*ResumableSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var session *ResumableSession
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("resumable_uploads"))
		data := bucket.Get([]byte(uploadID))
		if data == nil {
			return ErrKeyNotFound
		}
		return json.Unmarshal(data, &session)
	})

	return session, err
}

func (s *BoltDBMetadataStore) DeleteResumableSession(ctx context.Context, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("resumable_uploads"))
		return bucket.Delete([]byte(uploadID))
	})
}

func (s *BoltDBMetadataStore) ListExpiredSessions(ctx context.Context) ([]*ResumableSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*ResumableSession
	now := time.Now()

	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("resumable_uploads"))
		return bucket.ForEach(func(k, v []byte) error {
			var session ResumableSession
			if err := json.Unmarshal(v, &session); err == nil {
				if session.ExpiresAt.Before(now) && !session.Finalized {
					results = append(results, &session)
				}
			}
			return nil
		})
	})

	return results, err
}
