package gateway

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"cipherlake/internal/common"
	"cipherlake/internal/metadata"
)

type activeObjectMetadataInput struct {
	Bucket               string
	Key                  string
	Size                 int64
	ContentType          string
	ETag                 string
	UserMetadata         map[string]string
	StorageTier          common.StorageTier
	Encrypted            bool
	VersionID            string
	Checksum             string
	ChecksumType         string
	EncryptedDEK         []byte
	SSECUsed             bool
	SSECKeySHA256        string
	ServerSideEncryption string
	Now                  time.Time
}

func buildActiveObjectMetadata(input activeObjectMetadataInput) (*common.ObjectMetadata, *metadata.ObjectMetadata) {
	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}
	versionID := input.VersionID
	if versionID == "" {
		versionID = uuid.New().String()
	}
	storageTier := input.StorageTier
	if storageTier == 0 {
		storageTier = common.TierHot
	}

	commonMeta := &common.ObjectMetadata{
		Key:            input.Key,
		Bucket:         input.Bucket,
		Size:           input.Size,
		ContentType:    input.ContentType,
		ETag:           input.ETag,
		UserMetadata:   input.UserMetadata,
		StorageTier:    storageTier,
		CreatedAt:      now,
		ModifiedAt:     now,
		AccessCount:    0,
		LastAccessedAt: now,
		Encrypted:      input.Encrypted,
		VersionID:      versionID,
	}

	storeMeta := &metadata.ObjectMetadata{
		Key:            input.Key,
		Bucket:         input.Bucket,
		Size:           input.Size,
		ContentType:    input.ContentType,
		ETag:           input.ETag,
		UserMetadata:   input.UserMetadata,
		StorageTier:    int(storageTier),
		CreatedAt:      now,
		ModifiedAt:     now,
		AccessCount:    0,
		LastAccessedAt: now,
		Encrypted:      input.Encrypted,
		VersionID:      versionID,
		IsLatest:       true,
		ObjectStatus:   "active",
		Checksum:       input.Checksum,
		ChecksumType:   input.ChecksumType,
		SSECUsed:       input.SSECUsed,
		SSECKeySHA256:  input.SSECKeySHA256,
		SSECAlgorithm:  input.ServerSideEncryption,
	}

	if input.EncryptedDEK != nil {
		storeMeta.EncryptedDEK = input.EncryptedDEK
	}

	return commonMeta, storeMeta
}

func (g *S3Gateway) putStoredObjectWithMetadata(
	ctx context.Context,
	bucket, key string,
	data io.Reader,
	size int64,
	storageTier common.StorageTier,
	commonMeta *common.ObjectMetadata,
	storeMeta *metadata.ObjectMetadata,
	storeErrMessage string,
) error {
	if err := g.store.Put(ctx, bucket, key, data, size, storageTier, commonMeta); err != nil {
		return fmt.Errorf("%s: %w", storeErrMessage, err)
	}

	return g.putObjectMetadataAfterStore(ctx, bucket, key, storageTier, storeMeta)
}

func (g *S3Gateway) putObjectMetadataAfterStore(
	ctx context.Context,
	bucket, key string,
	storageTier common.StorageTier,
	storeMeta *metadata.ObjectMetadata,
) error {
	if err := g.metadata.PutObject(ctx, bucket, key, storeMeta); err != nil {
		g.store.Delete(ctx, bucket, key, storageTier)
		return fmt.Errorf("failed to store metadata: %w", err)
	}

	return nil
}
