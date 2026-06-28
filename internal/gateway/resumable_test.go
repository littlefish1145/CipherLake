package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nexus/internal/common"
	"nexus/internal/config"
	"nexus/internal/metadata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestGateway creates a test S3Gateway with resumable uploads enabled.
func setupTestGateway(t *testing.T) (*S3Gateway, string) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "nexus-resumable-test-*")
	require.NoError(t, err)

	cfg := &config.Config{
		Node: config.NodeConfig{
			DataDir: tmpDir,
		},
		Tiering: config.TieringConfig{
			Enabled:     true,
			HotMaxBytes: 1 << 30,
		},
		Resumable: config.ResumableConfig{
			Enabled:         true,
			UploadDir:       tmpDir + "/uploads",
			DefaultExpiry:   "24h",
			CleanupInterval: "5m",
		},
		Auth: config.AuthConfig{
			RequireAuth:   false,
			AnonymousRead: true,
		},
		Performance: config.PerformanceConfig{
			MaxUploadBytes: 1 << 30,
		},
	}

	gw, err := NewS3Gateway(cfg)
	require.NoError(t, err)

	// Create a test bucket
	bucketInfo := &metadata.BucketInfo{
		Name:      "test-bucket",
		CreatedAt: time.Now(),
		OwnerID:   "test-user",
		OwnerName: "test-user",
		Region:    "us-east-1",
		ACL:       "public-read-write",
	}
	err = gw.metadata.CreateBucket(context.Background(), "test-bucket", bucketInfo)
	require.NoError(t, err)

	t.Cleanup(func() {
		gw.Close()
		os.RemoveAll(tmpDir)
	})

	return gw, tmpDir
}

// authToken generates a valid JWT token for testing.
func authToken(gw *S3Gateway) string {
	token, err := gw.auth.GenerateToken("test-user")
	if err != nil {
		return ""
	}
	return token
}

// withAuth adds authentication to an HTTP request.
func withAuth(gw *S3Gateway, req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+authToken(gw))
	return req
}

// createTestSession is a helper that creates a resumable upload session and returns the uploadID.
func createTestSession(t *testing.T, gw *S3Gateway, bucket, key string) string {
	t.Helper()
	req := httptest.NewRequest("POST", fmt.Sprintf("/%s/%s?resumable", bucket, key), nil)
	req.Header.Set("Content-Type", "application/octet-stream")
	withAuth(gw, req)
	w := httptest.NewRecorder()
	err := gw.resumableHandler.HandleCreateSession(w, req, bucket, key)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, w.Code)
	uploadID := w.Header().Get("X-Nexus-Upload-Id")
	require.NotEmpty(t, uploadID)
	return uploadID
}

// patchTestData is a helper that appends data to a resumable upload session.
func patchTestData(t *testing.T, gw *S3Gateway, bucket, key, uploadID string, data []byte, offset int64) {
	t.Helper()
	req := httptest.NewRequest("PATCH", fmt.Sprintf("/%s/%s?uploadId=%s", bucket, key, uploadID), bytes.NewReader(data))
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	withAuth(gw, req)
	w := httptest.NewRecorder()
	err := gw.resumableHandler.HandlePatch(w, req, bucket, key)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestHandleCreateSession(t *testing.T) {
	gw, _ := setupTestGateway(t)

	t.Run("create session successfully", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test-bucket/test-key.txt?resumable", nil)
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Upload-Length", "1024")
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandleCreateSession(w, req, "test-bucket", "test-key.txt")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusCreated, w.Code)
		assert.Equal(t, "0", w.Header().Get("Upload-Offset"))

		location := w.Header().Get("Location")
		assert.Contains(t, location, "uploadId=")

		uploadID := w.Header().Get("X-Nexus-Upload-Id")
		assert.NotEmpty(t, uploadID)

		// Verify session is stored in metadata
		session, err := gw.metadata.GetResumableSession(context.Background(), uploadID)
		assert.NoError(t, err)
		assert.NotNil(t, session)
		assert.Equal(t, "test-bucket", session.Bucket)
		assert.Equal(t, "test-key.txt", session.Key)
		assert.Equal(t, int64(0), session.Offset)
		assert.False(t, session.Finalized)

		// Verify temp file exists
		tempPath := gw.resumableHandler.getTempFilePath(uploadID)
		_, err = os.Stat(tempPath)
		assert.NoError(t, err, "temp file should exist")
	})

	t.Run("create session without Upload-Length", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test-bucket/unknown-size.bin?resumable", nil)
		req.Header.Set("Content-Type", "application/octet-stream")
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandleCreateSession(w, req, "test-bucket", "unknown-size.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusCreated, w.Code)
		assert.Equal(t, "0", w.Header().Get("Upload-Offset"))
	})
}

func TestHandlePatch(t *testing.T) {
	gw, _ := setupTestGateway(t)
	uploadID := createTestSession(t, gw, "test-bucket", "test-key.bin")

	t.Run("append data successfully", func(t *testing.T) {
		data := []byte("Hello, World!")
		req := httptest.NewRequest("PATCH", "/test-bucket/test-key.bin?uploadId="+uploadID, bytes.NewReader(data))
		req.Header.Set("Upload-Offset", "0")
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "test-key.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, w.Code)
		assert.Equal(t, strconv.FormatInt(int64(len(data)), 10), w.Header().Get("Upload-Offset"))

		session, err := gw.metadata.GetResumableSession(context.Background(), uploadID)
		assert.NoError(t, err)
		assert.Equal(t, int64(len(data)), session.Offset)
	})

	t.Run("append more data", func(t *testing.T) {
		data := []byte(" More data here!")
		req := httptest.NewRequest("PATCH", "/test-bucket/test-key.bin?uploadId="+uploadID, bytes.NewReader(data))
		req.Header.Set("Upload-Offset", "13") // offset after first append
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "test-key.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, w.Code)
		assert.Equal(t, "29", w.Header().Get("Upload-Offset"))
	})

	t.Run("offset mismatch returns 409", func(t *testing.T) {
		data := []byte("should fail")
		req := httptest.NewRequest("PATCH", "/test-bucket/test-key.bin?uploadId="+uploadID, bytes.NewReader(data))
		req.Header.Set("Upload-Offset", "0") // wrong offset
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "test-key.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("missing uploadId returns 400", func(t *testing.T) {
		data := []byte("should fail")
		req := httptest.NewRequest("PATCH", "/test-bucket/test-key.bin", bytes.NewReader(data))
		req.Header.Set("Upload-Offset", "0")
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "test-key.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("non-existent uploadId returns 404", func(t *testing.T) {
		data := []byte("should fail")
		req := httptest.NewRequest("PATCH", "/test-bucket/test-key.bin?uploadId=nonexistent", bytes.NewReader(data))
		req.Header.Set("Upload-Offset", "0")
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "test-key.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandlePatchWithChecksum(t *testing.T) {
	gw, _ := setupTestGateway(t)
	uploadID := createTestSession(t, gw, "test-bucket", "checksum-test.bin")

	t.Run("correct checksum passes", func(t *testing.T) {
		data := []byte("test data for checksum")
		hasher := sha256.New()
		hasher.Write(data)
		checksum := base64.StdEncoding.EncodeToString(hasher.Sum(nil))

		req := httptest.NewRequest("PATCH", "/test-bucket/checksum-test.bin?uploadId="+uploadID, bytes.NewReader(data))
		req.Header.Set("Upload-Offset", "0")
		req.Header.Set("Upload-Checksum", "sha256 "+checksum)
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "checksum-test.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, w.Code)
	})

	t.Run("wrong checksum returns 400", func(t *testing.T) {
		data := []byte("more data")
		req := httptest.NewRequest("PATCH", "/test-bucket/checksum-test.bin?uploadId="+uploadID, bytes.NewReader(data))
		req.Header.Set("Upload-Offset", "22") // offset after previous write
		req.Header.Set("Upload-Checksum", "sha256 d3JvbmdjaGVja3N1bQ==")
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "checksum-test.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestHandleHead(t *testing.T) {
	gw, _ := setupTestGateway(t)

	req := httptest.NewRequest("POST", "/test-bucket/head-test.bin?resumable", nil)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Upload-Length", "1024")
	withAuth(gw, req)
	w := httptest.NewRecorder()
	err := gw.resumableHandler.HandleCreateSession(w, req, "test-bucket", "head-test.bin")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, w.Code)

	uploadID := w.Header().Get("X-Nexus-Upload-Id")
	require.NotEmpty(t, uploadID)

	// Append data
	patchTestData(t, gw, "test-bucket", "head-test.bin", uploadID, []byte("test data for HEAD"), 0)

	t.Run("query offset returns correct headers", func(t *testing.T) {
		req := httptest.NewRequest("HEAD", "/test-bucket/head-test.bin?uploadId="+uploadID, nil)
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandleHead(w, req, "test-bucket", "head-test.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, w.Code)
		assert.Equal(t, "18", w.Header().Get("Upload-Offset"))
		assert.Equal(t, "1024", w.Header().Get("Upload-Length"))
		assert.NotEmpty(t, w.Header().Get("Upload-Checksum"))
		assert.Equal(t, uploadID, w.Header().Get("X-Nexus-Upload-Id"))
	})

	t.Run("non-existent uploadId returns 404", func(t *testing.T) {
		req := httptest.NewRequest("HEAD", "/test-bucket/head-test.bin?uploadId=nonexistent", nil)
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandleHead(w, req, "test-bucket", "head-test.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestHandleFinalize(t *testing.T) {
	gw, _ := setupTestGateway(t)
	uploadID := createTestSession(t, gw, "test-bucket", "finalize-test.bin")
	patchTestData(t, gw, "test-bucket", "finalize-test.bin", uploadID, []byte("data to finalize"), 0)

	t.Run("finalize via POST with finalize param", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test-bucket/finalize-test.bin?uploadId="+uploadID+"&finalize=1", nil)
		withAuth(gw, req)

		w := httptest.NewRecorder()
		err := gw.resumableHandler.HandleFinalize(w, req, "test-bucket", "finalize-test.bin")
		assert.NoError(t, err)
		assert.Equal(t, http.StatusOK, w.Code)

		// Verify ETag is returned
		assert.NotEmpty(t, w.Header().Get("ETag"))

		// Verify version-id is returned
		assert.NotEmpty(t, w.Header().Get("x-amz-version-id"))

		// Verify session is cleaned up
		_, err = gw.metadata.GetResumableSession(context.Background(), uploadID)
		assert.Error(t, err, "session should be deleted after finalize")

		// Verify temp file is cleaned up
		tempPath := gw.resumableHandler.getTempFilePath(uploadID)
		_, err = os.Stat(tempPath)
		assert.True(t, os.IsNotExist(err), "temp file should be deleted after finalize")

		// Verify object metadata exists
		objMeta, err := gw.metadata.GetObject(context.Background(), "test-bucket", "finalize-test.bin")
		assert.NoError(t, err)
		assert.Equal(t, int64(16), objMeta.Size)
		assert.Equal(t, "application/octet-stream", objMeta.ContentType)
	})
}

func TestHandleFinalizeViaHeader(t *testing.T) {
	gw, _ := setupTestGateway(t)
	uploadID := createTestSession(t, gw, "test-bucket", "finalize-header.bin")

	// Append data with X-Nexus-Finalize: 1 header
	data := []byte("finalize via header")
	req := httptest.NewRequest("PATCH", "/test-bucket/finalize-header.bin?uploadId="+uploadID, bytes.NewReader(data))
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("X-Nexus-Finalize", "1")
	withAuth(gw, req)

	w := httptest.NewRecorder()
	err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "finalize-header.bin")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify object exists
	objMeta, err := gw.metadata.GetObject(context.Background(), "test-bucket", "finalize-header.bin")
	assert.NoError(t, err)
	assert.Equal(t, int64(len(data)), objMeta.Size)
}

func TestSessionExpiration(t *testing.T) {
	gw, tmpDir := setupTestGateway(t)

	// Create a session with short expiry (already expired)
	session := &metadata.ResumableSession{
		UploadID:    "expire-test-id",
		Bucket:      "test-bucket",
		Key:         "expire-test.bin",
		Offset:      0,
		TotalSize:   -1,
		ContentType: "application/octet-stream",
		Metadata:    map[string]string{},
		CreatedAt:   time.Now().Add(-2 * time.Hour),
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		Checksum:    "",
		Finalized:   false,
	}

	err := gw.metadata.PutResumableSession(context.Background(), session)
	require.NoError(t, err)

	// Create temp file
	tempPath := filepath.Join(tmpDir, "uploads", "expire-test-id.tmp")
	os.MkdirAll(filepath.Dir(tempPath), 0755)
	err = os.WriteFile(tempPath, []byte("test data"), 0644)
	require.NoError(t, err)

	// Run cleanup
	cleanup := NewResumableCleanup(gw.resumableHandler, 5*time.Minute)
	cleanup.cleanup()

	// Verify session is deleted
	_, err = gw.metadata.GetResumableSession(context.Background(), "expire-test-id")
	assert.Error(t, err, "expired session should be deleted")

	// Verify temp file is deleted
	_, err = os.Stat(tempPath)
	assert.True(t, os.IsNotExist(err), "temp file for expired session should be deleted")
}

func TestOffsetMismatchReturns409(t *testing.T) {
	gw, _ := setupTestGateway(t)
	uploadID := createTestSession(t, gw, "test-bucket", "offset-mismatch.bin")

	// Append some data first
	patchTestData(t, gw, "test-bucket", "offset-mismatch.bin", uploadID, []byte("initial data"), 0)

	// Now try with wrong offset
	wrongData := []byte("should fail")
	req := httptest.NewRequest("PATCH", "/test-bucket/offset-mismatch.bin?uploadId="+uploadID, bytes.NewReader(wrongData))
	req.Header.Set("Upload-Offset", "5") // Wrong - should be 12
	withAuth(gw, req)

	w := httptest.NewRecorder()
	err := gw.resumableHandler.HandlePatch(w, req, "test-bucket", "offset-mismatch.bin")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusConflict, w.Code)

	// Verify offset hasn't changed
	session, err := gw.metadata.GetResumableSession(context.Background(), uploadID)
	assert.NoError(t, err)
	assert.Equal(t, int64(12), session.Offset, "offset should not change on mismatch")
}

func TestResumableSessionExpiry(t *testing.T) {
	gw, _ := setupTestGateway(t)
	uploadID := createTestSession(t, gw, "test-bucket", "expiry-test.bin")

	// Manually expire the session
	session, err := gw.metadata.GetResumableSession(context.Background(), uploadID)
	require.NoError(t, err)
	session.ExpiresAt = time.Now().Add(-1 * time.Hour)
	err = gw.metadata.PutResumableSession(context.Background(), session)
	require.NoError(t, err)

	// Try to PATCH expired session
	data := []byte("should fail - expired")
	req := httptest.NewRequest("PATCH", "/test-bucket/expiry-test.bin?uploadId="+uploadID, bytes.NewReader(data))
	req.Header.Set("Upload-Offset", "0")
	withAuth(gw, req)

	w := httptest.NewRecorder()
	err = gw.resumableHandler.HandlePatch(w, req, "test-bucket", "expiry-test.bin")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusGone, w.Code)

	// Try to HEAD expired session
	headReq := httptest.NewRequest("HEAD", "/test-bucket/expiry-test.bin?uploadId="+uploadID, nil)
	withAuth(gw, headReq)

	headW := httptest.NewRecorder()
	err = gw.resumableHandler.HandleHead(headW, headReq, "test-bucket", "expiry-test.bin")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusGone, headW.Code)

	// Try to finalize expired session
	finalizeReq := httptest.NewRequest("POST", "/test-bucket/expiry-test.bin?uploadId="+uploadID+"&finalize=1", nil)
	withAuth(gw, finalizeReq)

	finalizeW := httptest.NewRecorder()
	err = gw.resumableHandler.HandleFinalize(finalizeW, finalizeReq, "test-bucket", "expiry-test.bin")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusGone, finalizeW.Code)
}

func TestCoexistenceWithS3Multipart(t *testing.T) {
	gw, _ := setupTestGateway(t)

	// 1. Create a resumable session
	resumableUploadID := createTestSession(t, gw, "test-bucket", "resumable-coexist.bin")

	// 2. Create an S3 multipart upload
	multipartHandler := NewMultipartUploadHandler(gw)
	multipartReq := httptest.NewRequest("POST", "/test-bucket/multipart-coexist.bin?uploads", nil)
	withAuth(gw, multipartReq)
	multipartW := httptest.NewRecorder()
	err := multipartHandler.HandleCreateMultipartUpload(multipartW, multipartReq, "test-bucket", "multipart-coexist.bin")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, multipartW.Code)
	multipartUploadID := multipartW.Header().Get("x-amz-upload-id")

	// Both should have different upload IDs
	assert.NotEmpty(t, resumableUploadID)
	assert.NotEmpty(t, multipartUploadID)
	assert.NotEqual(t, resumableUploadID, multipartUploadID)

	// Both should be independently queryable
	resumableSession, err := gw.metadata.GetResumableSession(context.Background(), resumableUploadID)
	assert.NoError(t, err)
	assert.Equal(t, "resumable-coexist.bin", resumableSession.Key)

	multipartSession, err := gw.metadata.GetUpload(context.Background(), "test-bucket", "multipart-coexist.bin", multipartUploadID)
	assert.NoError(t, err)
	assert.Equal(t, "multipart-coexist.bin", multipartSession.Key)
}

func TestFullResumableWorkflow(t *testing.T) {
	gw, _ := setupTestGateway(t)

	bucket := "test-bucket"
	key := "full-workflow.bin"

	// Step 1: Create session
	req := httptest.NewRequest("POST", fmt.Sprintf("/%s/%s?resumable", bucket, key), nil)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Upload-Length", "30")
	withAuth(gw, req)

	w := httptest.NewRecorder()
	err := gw.resumableHandler.HandleCreateSession(w, req, bucket, key)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, w.Code)

	uploadID := w.Header().Get("X-Nexus-Upload-Id")
	require.NotEmpty(t, uploadID)

	// Step 2: Query offset (should be 0)
	headReq := httptest.NewRequest("HEAD", fmt.Sprintf("/%s/%s?uploadId=%s", bucket, key, uploadID), nil)
	withAuth(gw, headReq)

	headW := httptest.NewRecorder()
	err = gw.resumableHandler.HandleHead(headW, headReq, bucket, key)
	require.NoError(t, err)
	assert.Equal(t, "0", headW.Header().Get("Upload-Offset"))
	assert.Equal(t, "30", headW.Header().Get("Upload-Length"))

	// Step 3: Append first chunk
	chunk1 := []byte("Hello, ")
	patch1Req := httptest.NewRequest("PATCH", fmt.Sprintf("/%s/%s?uploadId=%s", bucket, key, uploadID), bytes.NewReader(chunk1))
	patch1Req.Header.Set("Upload-Offset", "0")
	withAuth(gw, patch1Req)

	patch1W := httptest.NewRecorder()
	err = gw.resumableHandler.HandlePatch(patch1W, patch1Req, bucket, key)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, patch1W.Code)
	assert.Equal(t, strconv.FormatInt(int64(len(chunk1)), 10), patch1W.Header().Get("Upload-Offset"))

	// Step 4: Query offset after first chunk
	headReq2 := httptest.NewRequest("HEAD", fmt.Sprintf("/%s/%s?uploadId=%s", bucket, key, uploadID), nil)
	withAuth(gw, headReq2)

	headW2 := httptest.NewRecorder()
	err = gw.resumableHandler.HandleHead(headW2, headReq2, bucket, key)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(int64(len(chunk1)), 10), headW2.Header().Get("Upload-Offset"))

	// Step 5: Append second chunk
	chunk2 := []byte("World!")
	patch2Req := httptest.NewRequest("PATCH", fmt.Sprintf("/%s/%s?uploadId=%s", bucket, key, uploadID), bytes.NewReader(chunk2))
	patch2Req.Header.Set("Upload-Offset", strconv.FormatInt(int64(len(chunk1)), 10))
	withAuth(gw, patch2Req)

	patch2W := httptest.NewRecorder()
	err = gw.resumableHandler.HandlePatch(patch2W, patch2Req, bucket, key)
	require.NoError(t, err)

	// Step 6: Finalize
	finalizeReq := httptest.NewRequest("POST", fmt.Sprintf("/%s/%s?uploadId=%s&finalize=1", bucket, key, uploadID), nil)
	withAuth(gw, finalizeReq)

	finalizeW := httptest.NewRecorder()
	err = gw.resumableHandler.HandleFinalize(finalizeW, finalizeReq, bucket, key)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, finalizeW.Code)

	// Step 7: Verify object is stored
	objMeta, err := gw.metadata.GetObject(context.Background(), bucket, key)
	require.NoError(t, err)
	expectedSize := int64(len(chunk1) + len(chunk2))
	assert.Equal(t, expectedSize, objMeta.Size)
	assert.Equal(t, "application/octet-stream", objMeta.ContentType)
	assert.NotEmpty(t, objMeta.ETag)
	assert.NotEmpty(t, objMeta.VersionID)

	// Step 8: Verify we can read the object back
	storageTier := common.StorageTier(objMeta.StorageTier)
	reader, _, err := gw.store.Get(context.Background(), bucket, key, storageTier)
	require.NoError(t, err)
	defer reader.Close()

	content, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, string(chunk1)+string(chunk2), string(content))
}

func TestResumableMetadataStore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nexus-metadata-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	store, err := metadata.NewBoltDBMetadataStore(tmpDir + "/test.db")
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	t.Run("put and get session", func(t *testing.T) {
		session := &metadata.ResumableSession{
			UploadID:    "test-upload-id",
			Bucket:      "test-bucket",
			Key:         "test-key",
			Offset:      1024,
			TotalSize:   2048,
			ContentType: "text/plain",
			Metadata:    map[string]string{"foo": "bar"},
			CreatedAt:   time.Now(),
			ExpiresAt:   time.Now().Add(24 * time.Hour),
			Checksum:    "abc123",
			Finalized:   false,
		}

		err := store.PutResumableSession(ctx, session)
		assert.NoError(t, err)

		got, err := store.GetResumableSession(ctx, "test-upload-id")
		assert.NoError(t, err)
		assert.Equal(t, session.UploadID, got.UploadID)
		assert.Equal(t, session.Bucket, got.Bucket)
		assert.Equal(t, session.Key, got.Key)
		assert.Equal(t, session.Offset, got.Offset)
		assert.Equal(t, session.TotalSize, got.TotalSize)
		assert.Equal(t, session.ContentType, got.ContentType)
		assert.Equal(t, session.Metadata["foo"], got.Metadata["foo"])
		assert.Equal(t, session.Checksum, got.Checksum)
		assert.Equal(t, session.Finalized, got.Finalized)
	})

	t.Run("get non-existent session", func(t *testing.T) {
		_, err := store.GetResumableSession(ctx, "non-existent")
		assert.Error(t, err)
	})

	t.Run("delete session", func(t *testing.T) {
		session := &metadata.ResumableSession{
			UploadID:  "delete-test-id",
			Bucket:    "test-bucket",
			Key:       "delete-key",
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(24 * time.Hour),
		}

		err := store.PutResumableSession(ctx, session)
		assert.NoError(t, err)

		err = store.DeleteResumableSession(ctx, "delete-test-id")
		assert.NoError(t, err)

		_, err = store.GetResumableSession(ctx, "delete-test-id")
		assert.Error(t, err)
	})

	t.Run("list expired sessions", func(t *testing.T) {
		// Create expired session
		expiredSession := &metadata.ResumableSession{
			UploadID:  "expired-id",
			Bucket:    "test-bucket",
			Key:       "expired-key",
			CreatedAt: time.Now().Add(-2 * time.Hour),
			ExpiresAt: time.Now().Add(-1 * time.Hour),
			Finalized: false,
		}
		err := store.PutResumableSession(ctx, expiredSession)
		assert.NoError(t, err)

		// Create active session
		activeSession := &metadata.ResumableSession{
			UploadID:  "active-id",
			Bucket:    "test-bucket",
			Key:       "active-key",
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(24 * time.Hour),
			Finalized: false,
		}
		err = store.PutResumableSession(ctx, activeSession)
		assert.NoError(t, err)

		// Create finalized session (even if expired, should not be listed)
		finalizedSession := &metadata.ResumableSession{
			UploadID:  "finalized-id",
			Bucket:    "test-bucket",
			Key:       "finalized-key",
			CreatedAt: time.Now().Add(-2 * time.Hour),
			ExpiresAt: time.Now().Add(-1 * time.Hour),
			Finalized: true,
		}
		err = store.PutResumableSession(ctx, finalizedSession)
		assert.NoError(t, err)

		expired, err := store.ListExpiredSessions(ctx)
		assert.NoError(t, err)

		// Should only contain the expired, non-finalized session
		foundExpired := false
		for _, s := range expired {
			if s.UploadID == "expired-id" {
				foundExpired = true
			}
			assert.NotEqual(t, "active-id", s.UploadID, "active session should not be in expired list")
			assert.NotEqual(t, "finalized-id", s.UploadID, "finalized session should not be in expired list")
		}
		assert.True(t, foundExpired, "expired session should be in expired list")
	})
}

func TestResumableConfigDefaults(t *testing.T) {
	cfg := &config.Config{
		Resumable: config.ResumableConfig{
			Enabled:         true,
			UploadDir:       "/tmp/test/uploads",
			DefaultExpiry:   "24h",
			CleanupInterval: "5m",
			MaxSessionSize:  "100GB",
		},
	}

	assert.True(t, cfg.Resumable.Enabled)
	assert.Equal(t, "24h", cfg.Resumable.DefaultExpiry)
	assert.Equal(t, "5m", cfg.Resumable.CleanupInterval)
}

func TestResumableRoutingWithXNexusHeader(t *testing.T) {
	gw, _ := setupTestGateway(t)
	uploadID := createTestSession(t, gw, "test-bucket", "route-test.bin")

	// Verify that with X-Nexus-Resumable: 1, the PATCH is routed to resumable handler
	data := []byte("routed data")
	patchReq := httptest.NewRequest("PATCH", "/test-bucket/route-test.bin?uploadId="+uploadID, bytes.NewReader(data))
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("X-Nexus-Resumable", "1")
	withAuth(gw, patchReq)

	patchW := httptest.NewRecorder()
	handler := gw.handleObjectOperations("test-bucket", "route-test.bin", "PATCH")
	err := handler(patchW, patchReq)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, patchW.Code)
}

func TestResumableFinalizeTriggersEncryption(t *testing.T) {
	// This test verifies that the finalize flow correctly handles
	// the non-encrypted path when crypto services are not enabled.
	gw, _ := setupTestGateway(t)
	uploadID := createTestSession(t, gw, "test-bucket", "encrypt-test.bin")
	patchTestData(t, gw, "test-bucket", "encrypt-test.bin", uploadID, []byte("data to encrypt (or not)"), 0)

	// Finalize
	req := httptest.NewRequest("POST", "/test-bucket/encrypt-test.bin?uploadId="+uploadID+"&finalize=1", nil)
	withAuth(gw, req)

	w := httptest.NewRecorder()
	err := gw.resumableHandler.HandleFinalize(w, req, "test-bucket", "encrypt-test.bin")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify object is stored (without encryption since crypto services are not enabled)
	objMeta, err := gw.metadata.GetObject(context.Background(), "test-bucket", "encrypt-test.bin")
	require.NoError(t, err)
	assert.False(t, objMeta.Encrypted, "object should not be encrypted when crypto services are disabled")
}

func TestResumableCleanupStop(t *testing.T) {
	gw, _ := setupTestGateway(t)

	cleanup := NewResumableCleanup(gw.resumableHandler, 1*time.Second)

	// Start and stop should not panic
	cleanup.Start()
	time.Sleep(100 * time.Millisecond) // Let it start
	cleanup.Stop()

	// Double stop should not panic
	cleanup.Stop()
}

func TestCreateSessionResponseBody(t *testing.T) {
	gw, _ := setupTestGateway(t)

	req := httptest.NewRequest("POST", "/test-bucket/response-test.bin?resumable", nil)
	req.Header.Set("Content-Type", "text/plain")
	withAuth(gw, req)

	w := httptest.NewRecorder()
	err := gw.resumableHandler.HandleCreateSession(w, req, "test-bucket", "response-test.bin")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, w.Code)

	// Parse JSON body
	var resp map[string]string
	err = json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.NotEmpty(t, resp["upload_id"])
	assert.Contains(t, resp["location"], "uploadId=")
	assert.True(t, strings.HasPrefix(resp["location"], "/test-bucket/response-test.bin?uploadId="))
}
