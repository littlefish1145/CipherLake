package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cipherlake/internal/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupExpiredUploadsDeletesExpiredUpload(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	ctx := context.Background()
	err := gateway.metadata.PutUpload(ctx, &metadata.MultipartUpload{
		UploadID:  "expired-upload",
		Bucket:    "test-bucket",
		Key:       "expired.txt",
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)

	err = gateway.multipartSvc.handler.CleanupExpiredUploads(ctx)
	require.NoError(t, err)

	_, err = gateway.metadata.GetUpload(ctx, "test-bucket", "expired.txt", "expired-upload")
	assert.ErrorIs(t, err, metadata.ErrKeyNotFound)
}

func TestMultipartCleanupStopIsIdempotent(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	cleanup := NewMultipartCleanup(gateway.multipartSvc.handler, time.Hour)
	cleanup.Start()
	cleanup.Stop()
	cleanup.Stop()
}

func TestS3GatewayCloseClosesMetadataStore(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	require.NoError(t, gateway.Close())

	_, err := gateway.metadata.GetBucket(context.Background(), "test-bucket")
	require.Error(t, err)

	require.NoError(t, gateway.Close())
}

// TestMultipartCleanupPartsRemovesPartFiles verifies that cleanupParts
// actually removes the part files and the upload directory from disk.
func TestMultipartCleanupPartsRemovesPartFiles(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	handler := gateway.multipartSvc.handler

	uploadID := "test-upload-cleanup"
	uploadDir := handler.getUploadDir(uploadID)
	require.NoError(t, os.MkdirAll(uploadDir, 0o755))

	partPath := handler.getPartPath(uploadID, 1)
	require.NoError(t, os.WriteFile(partPath, []byte("part-data"), 0o644))

	_, err := os.Stat(partPath)
	require.NoError(t, err)

	require.NoError(t, handler.cleanupParts(uploadID))

	_, err = os.Stat(partPath)
	assert.True(t, os.IsNotExist(err), "part file should be removed")
	_, err = os.Stat(uploadDir)
	assert.True(t, os.IsNotExist(err), "upload directory should be removed")
}

// TestMultipartCleanupPartsIsIdempotent verifies that calling cleanupParts
// multiple times does not error when the directory is already gone.
func TestMultipartCleanupPartsIsIdempotent(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	handler := gateway.multipartSvc.handler

	uploadID := "test-upload-idempotent"

	// First call on a non-existent directory should not panic and should
	// return nil (because os.IsNotExist is suppressed).
	assert.NotPanics(t, func() {
		require.NoError(t, handler.cleanupParts(uploadID))
	})
	// Second call should also be safe and idempotent.
	assert.NotPanics(t, func() {
		require.NoError(t, handler.cleanupParts(uploadID))
	})
}

// TestMultipartCleanupPartsRemovesStaleFilesInDir verifies that cleanupParts
// removes any stale files left in the upload directory (e.g., .tmp files).
func TestMultipartCleanupPartsRemovesStaleFilesInDir(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	handler := gateway.multipartSvc.handler

	uploadID := "test-upload-stale"
	uploadDir := handler.getUploadDir(uploadID)
	require.NoError(t, os.MkdirAll(uploadDir, 0o755))

	stalePath := filepath.Join(uploadDir, "part-1.tmp")
	require.NoError(t, os.WriteFile(stalePath, []byte("stale"), 0o644))

	require.NoError(t, handler.cleanupParts(uploadID))

	_, err := os.Stat(stalePath)
	assert.True(t, os.IsNotExist(err))
}
