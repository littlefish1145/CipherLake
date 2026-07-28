package gateway

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"cipherlake/internal/common"
)

func TestBuildActiveObjectMetadataPopulatesBothMetadataShapes(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	userMetadata := map[string]string{"foo": "bar"}
	encryptedDEK := []byte("dek")

	commonMeta, storeMeta := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:               "bucket",
		Key:                  "key",
		Size:                 42,
		ContentType:          "text/plain",
		ETag:                 "etag",
		UserMetadata:         userMetadata,
		StorageTier:          common.TierWarm,
		Encrypted:            true,
		VersionID:            "version",
		Checksum:             "checksum",
		ChecksumType:         "sha256",
		EncryptedDEK:         encryptedDEK,
		SSECUsed:             true,
		SSECKeySHA256:        "key-sha",
		ServerSideEncryption: "AES256",
		Now:                  now,
	})

	if commonMeta.VersionID != "version" || storeMeta.VersionID != "version" {
		t.Fatalf("version ids = %q/%q, want version", commonMeta.VersionID, storeMeta.VersionID)
	}
	if !commonMeta.CreatedAt.Equal(now) || !storeMeta.CreatedAt.Equal(now) {
		t.Fatalf("created timestamps = %v/%v, want %v", commonMeta.CreatedAt, storeMeta.CreatedAt, now)
	}
	if !commonMeta.ModifiedAt.Equal(now) || !storeMeta.ModifiedAt.Equal(now) {
		t.Fatalf("modified timestamps = %v/%v, want %v", commonMeta.ModifiedAt, storeMeta.ModifiedAt, now)
	}
	if !commonMeta.LastAccessedAt.Equal(now) || !storeMeta.LastAccessedAt.Equal(now) {
		t.Fatalf("last accessed timestamps = %v/%v, want %v", commonMeta.LastAccessedAt, storeMeta.LastAccessedAt, now)
	}
	if commonMeta.StorageTier != common.TierWarm || storeMeta.StorageTier != int(common.TierWarm) {
		t.Fatalf("storage tiers = %v/%v, want warm", commonMeta.StorageTier, storeMeta.StorageTier)
	}
	if storeMeta.ObjectStatus != "active" || !storeMeta.IsLatest {
		t.Fatalf("store metadata status = %q latest=%v, want active/latest", storeMeta.ObjectStatus, storeMeta.IsLatest)
	}
	if storeMeta.Checksum != "checksum" || storeMeta.ChecksumType != "sha256" {
		t.Fatalf("checksum = %q/%q, want checksum/sha256", storeMeta.Checksum, storeMeta.ChecksumType)
	}
	if !storeMeta.SSECUsed || storeMeta.SSECKeySHA256 != "key-sha" || storeMeta.SSECAlgorithm != "AES256" {
		t.Fatalf("SSE-C metadata not populated correctly")
	}
	if string(storeMeta.EncryptedDEK) != "dek" {
		t.Fatalf("encrypted DEK = %q, want dek", string(storeMeta.EncryptedDEK))
	}
}

func TestPutStoredObjectWithMetadataWritesObjectAndMetadata(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	ctx := context.Background()
	commonMeta, storeMeta := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:       "test-bucket",
		Key:          "stored.txt",
		Size:         5,
		ContentType:  "text/plain",
		ETag:         "etag",
		UserMetadata: map[string]string{"foo": "bar"},
		StorageTier:  common.TierHot,
	})

	err := gateway.putStoredObjectWithMetadata(
		ctx,
		"test-bucket",
		"stored.txt",
		bytes.NewReader([]byte("hello")),
		5,
		common.TierHot,
		commonMeta,
		storeMeta,
		"failed to store object",
	)
	if err != nil {
		t.Fatalf("put object with metadata: %v", err)
	}

	reader, _, err := gateway.store.Get(ctx, "test-bucket", "stored.txt", common.TierHot)
	if err != nil {
		t.Fatalf("read stored object: %v", err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read object body: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", string(body))
	}

	gotMeta, err := gateway.metadata.GetObject(ctx, "test-bucket", "stored.txt")
	if err != nil {
		t.Fatalf("read stored metadata: %v", err)
	}
	if gotMeta.ContentType != "text/plain" || gotMeta.UserMetadata["foo"] != "bar" {
		t.Fatalf("metadata = %+v, want content type and user metadata", gotMeta)
	}
}
