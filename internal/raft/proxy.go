package raft

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"nexus/internal/metadata"
)

// RaftMetadataProxy wraps BoltDBMetadataStore and routes writes through Raft
// when Raft is enabled. If Raft is not enabled, it falls through to direct
// BoltDB operations. Reads always go to the local BoltDB (optionally with
// linearizable read verification).
type RaftMetadataProxy struct {
	store    *metadata.BoltDBMetadataStore
	node     *RaftNode
	enabled  bool
}

// NewRaftMetadataProxy creates a new RaftMetadataProxy.
func NewRaftMetadataProxy(store *metadata.BoltDBMetadataStore, node *RaftNode, enabled bool) *RaftMetadataProxy {
	return &RaftMetadataProxy{
		store:   store,
		node:    node,
		enabled: enabled,
	}
}

// PutObject stores object metadata. If Raft is enabled, the operation goes
// through RaftApply; otherwise it falls through to direct BoltDB.
func (p *RaftMetadataProxy) PutObject(ctx context.Context, bucket, key string, md *metadata.ObjectMetadata) error {
	if !p.enabled || p.node == nil {
		return p.store.PutObject(ctx, bucket, key, md)
	}

	data, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("failed to marshal object metadata: %w", err)
	}

	op := &FSMOperation{
		Type:   "put_object",
		Bucket: bucket,
		Key:    key,
		Data:   data,
	}
	return p.node.RaftApply(ctx, op)
}

// GetObject retrieves object metadata. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) GetObject(ctx context.Context, bucket, key string) (*metadata.ObjectMetadata, error) {
	return p.store.GetObject(ctx, bucket, key)
}

// DeleteObject deletes object metadata. If Raft is enabled, the operation
// goes through RaftApply.
func (p *RaftMetadataProxy) DeleteObject(ctx context.Context, bucket, key string) error {
	if !p.enabled || p.node == nil {
		return p.store.DeleteObject(ctx, bucket, key)
	}

	op := &FSMOperation{
		Type:   "delete_object",
		Bucket: bucket,
		Key:    key,
	}
	return p.node.RaftApply(ctx, op)
}

// ListObjects lists objects. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) ListObjects(ctx context.Context, bucket, prefix string, maxKeys int) ([]*metadata.ObjectMetadata, error) {
	return p.store.ListObjects(ctx, bucket, prefix, maxKeys)
}

// ListObjectsWithDelimiter lists objects with delimiter. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) ListObjectsWithDelimiter(ctx context.Context, bucket, prefix, delimiter string, maxKeys int) (*metadata.ListResult, error) {
	return p.store.ListObjectsWithDelimiter(ctx, bucket, prefix, delimiter, maxKeys)
}

// CreateBucket creates a bucket. If Raft is enabled, the operation goes
// through RaftApply.
func (p *RaftMetadataProxy) CreateBucket(ctx context.Context, bucket string, info *metadata.BucketInfo) error {
	if !p.enabled || p.node == nil {
		return p.store.CreateBucket(ctx, bucket, info)
	}

	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to marshal bucket info: %w", err)
	}

	op := &FSMOperation{
		Type:   "create_bucket",
		Bucket: bucket,
		Data:   data,
	}
	return p.node.RaftApply(ctx, op)
}

// GetBucket retrieves bucket info. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) GetBucket(ctx context.Context, bucket string) (*metadata.BucketInfo, error) {
	return p.store.GetBucket(ctx, bucket)
}

// DeleteBucket deletes a bucket. If Raft is enabled, the operation goes
// through RaftApply.
func (p *RaftMetadataProxy) DeleteBucket(ctx context.Context, bucket string) error {
	if !p.enabled || p.node == nil {
		return p.store.DeleteBucket(ctx, bucket)
	}

	op := &FSMOperation{
		Type:   "delete_bucket",
		Bucket: bucket,
	}
	return p.node.RaftApply(ctx, op)
}

// ListBuckets lists all buckets. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) ListBuckets(ctx context.Context) ([]*metadata.BucketInfo, error) {
	return p.store.ListBuckets(ctx)
}

// UpdateBucket updates bucket info. If Raft is enabled, the operation goes
// through RaftApply.
func (p *RaftMetadataProxy) UpdateBucket(ctx context.Context, bucket string, info *metadata.BucketInfo) error {
	if !p.enabled || p.node == nil {
		return p.store.UpdateBucket(ctx, bucket, info)
	}

	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to marshal bucket info: %w", err)
	}

	op := &FSMOperation{
		Type:   "update_bucket",
		Bucket: bucket,
		Data:   data,
	}
	return p.node.RaftApply(ctx, op)
}

// PutObjectVersion stores an object version. If Raft is enabled, goes through RaftApply.
func (p *RaftMetadataProxy) PutObjectVersion(ctx context.Context, bucket, key string, md *metadata.ObjectMetadata) error {
	if !p.enabled || p.node == nil {
		return p.store.PutObjectVersion(ctx, bucket, key, md)
	}

	data, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("failed to marshal object metadata: %w", err)
	}

	op := &FSMOperation{
		Type:   "put_object_version",
		Bucket: bucket,
		Key:    key,
		Data:   data,
	}
	return p.node.RaftApply(ctx, op)
}

// GetObjectVersion retrieves a specific object version. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) GetObjectVersion(ctx context.Context, bucket, key, versionID string) (*metadata.ObjectMetadata, error) {
	return p.store.GetObjectVersion(ctx, bucket, key, versionID)
}

// ListObjectVersions lists object versions. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) ListObjectVersions(ctx context.Context, bucket, key string, maxVersions int) ([]*metadata.ObjectMetadata, error) {
	return p.store.ListObjectVersions(ctx, bucket, key, maxVersions)
}

// PutUpload stores a multipart upload. If Raft is enabled, goes through RaftApply.
func (p *RaftMetadataProxy) PutUpload(ctx context.Context, upload *metadata.MultipartUpload) error {
	if !p.enabled || p.node == nil {
		return p.store.PutUpload(ctx, upload)
	}

	data, err := json.Marshal(upload)
	if err != nil {
		return fmt.Errorf("failed to marshal upload: %w", err)
	}

	op := &FSMOperation{
		Type:   "put_upload",
		Bucket: upload.Bucket,
		Key:    upload.Key,
		Data:   data,
	}
	return p.node.RaftApply(ctx, op)
}

// GetUpload retrieves a multipart upload. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) GetUpload(ctx context.Context, bucket, key, uploadID string) (*metadata.MultipartUpload, error) {
	return p.store.GetUpload(ctx, bucket, key, uploadID)
}

// DeleteUpload deletes a multipart upload. If Raft is enabled, goes through RaftApply.
func (p *RaftMetadataProxy) DeleteUpload(ctx context.Context, bucket, key, uploadID string) error {
	if !p.enabled || p.node == nil {
		return p.store.DeleteUpload(ctx, bucket, key, uploadID)
	}

	op := &FSMOperation{
		Type:   "delete_upload",
		Bucket: bucket,
		Key:    key,
	}
	return p.node.RaftApply(ctx, op)
}

// ListUploads lists multipart uploads. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) ListUploads(ctx context.Context, bucket string) ([]*metadata.MultipartUpload, error) {
	return p.store.ListUploads(ctx, bucket)
}

// AddPart adds a part to a multipart upload. If Raft is enabled, goes through RaftApply.
func (p *RaftMetadataProxy) AddPart(ctx context.Context, uploadID string, part *metadata.UploadPart) error {
	if !p.enabled || p.node == nil {
		return p.store.AddPart(ctx, uploadID, part)
	}

	data, err := json.Marshal(part)
	if err != nil {
		return fmt.Errorf("failed to marshal upload part: %w", err)
	}

	op := &FSMOperation{
		Type:   "add_part",
		Bucket: uploadID,
		Key:    fmt.Sprintf("%d", part.PartNumber),
		Data:   data,
	}
	return p.node.RaftApply(ctx, op)
}

// GetParts retrieves parts for a multipart upload. Reads always go to local BoltDB.
func (p *RaftMetadataProxy) GetParts(ctx context.Context, uploadID string) ([]*metadata.UploadPart, error) {
	return p.store.GetParts(ctx, uploadID)
}

// Close closes the underlying store.
func (p *RaftMetadataProxy) Close() error {
	return p.store.Close()
}

// FlushWAL flushes the write-ahead log.
func (p *RaftMetadataProxy) FlushWAL() error {
	return p.store.FlushWAL()
}

// GetStats returns metadata statistics.
func (p *RaftMetadataProxy) GetStats() *metadata.MetadataStats {
	return p.store.GetStats()
}

// LinearizableRead performs a linearizable read verification if Raft is enabled.
func (p *RaftMetadataProxy) LinearizableRead(ctx context.Context) error {
	if !p.enabled || p.node == nil {
		return nil
	}
	return p.node.LinearizableRead(ctx)
}

// IsRaftEnabled returns whether Raft consensus is enabled.
func (p *RaftMetadataProxy) IsRaftEnabled() bool {
	return p.enabled
}

// GetRaftNode returns the underlying RaftNode (nil if Raft is not enabled).
func (p *RaftMetadataProxy) GetRaftNode() *RaftNode {
	return p.node
}

// Ensure RaftMetadataProxy implements metadata.MetadataStore at compile time.
var _ metadata.MetadataStore = (*RaftMetadataProxy)(nil)

// Unused import guard for time
var _ time.Duration
