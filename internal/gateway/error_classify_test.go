package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cipherlake/internal/common"
	"cipherlake/internal/metadata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyHandlerErrorMapsSentinelErrors verifies that the gateway's
// error classifier maps metadata sentinel errors to the correct S3 HTTP
// status codes and S3 error codes.
func TestClassifyHandlerErrorMapsSentinelErrors(t *testing.T) {
	gateway, _ := setupTestGateway(t)

	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "key not found",
			err:         fmt.Errorf("wrap: %w", metadata.ErrKeyNotFound),
			wantStatus:  http.StatusNotFound,
			wantCode:    "NoSuchKey",
			wantMessage: "The specified key does not exist.",
		},
		{
			name:        "bucket not found",
			err:         metadata.ErrBucketNotFound,
			wantStatus:  http.StatusNotFound,
			wantCode:    "NoSuchBucket",
			wantMessage: "The specified bucket does not exist.",
		},
		{
			name:        "bucket already exists",
			err:         metadata.ErrBucketAlreadyExists,
			wantStatus:  http.StatusConflict,
			wantCode:    "BucketAlreadyExists",
			wantMessage: "The requested bucket name is not available.",
		},
		{
			name:        "bucket not empty",
			err:         metadata.ErrBucketNotEmpty,
			wantStatus:  http.StatusConflict,
			wantCode:    "BucketNotEmpty",
			wantMessage: "The bucket you tried to delete is not empty.",
		},
		{
			name:        "invalid key",
			err:         metadata.ErrInvalidKey,
			wantStatus:  http.StatusBadRequest,
			wantCode:    "InvalidKey",
			wantMessage: "The specified key is not valid.",
		},
		{
			name:        "version not found",
			err:         metadata.ErrVersionNotFound,
			wantStatus:  http.StatusNotFound,
			wantCode:    "NoSuchVersion",
			wantMessage: "The specified version does not exist.",
		},
		{
			name:        "quota exceeded",
			err:         metadata.ErrQuotaExceeded,
			wantStatus:  http.StatusTooManyRequests,
			wantCode:    "QuotaExceeded",
			wantMessage: "The bucket quota has been exceeded.",
		},
		{
			name:        "transaction failed",
			err:         metadata.ErrTransactionFailed,
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    "TransactionFailed",
			wantMessage: "The metadata transaction could not be committed.",
		},
		{
			name:        "access denied prefix",
			err:         errors.New("access denied: caller not authorized"),
			wantStatus:  http.StatusForbidden,
			wantCode:    "AccessDenied",
			wantMessage: "access denied: caller not authorized",
		},
		{
			name:        "method not allowed prefix",
			err:         errors.New("method not allowed"),
			wantStatus:  http.StatusMethodNotAllowed,
			wantCode:    "MethodNotAllowed",
			wantMessage: "The specified method is not allowed against this resource.",
		},
		{
			name:        "uploadId required",
			err:         errors.New("uploadId is required"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    "InvalidRequest",
			wantMessage: "uploadId is required",
		},
		{
			name:        "generic error",
			err:         errors.New("some unexpected failure"),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    "InternalError",
			wantMessage: "An internal error occurred",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, message := gateway.classifyHandlerError(tc.err)
			assert.Equal(t, tc.wantStatus, status, "status mismatch")
			assert.Equal(t, tc.wantCode, code, "code mismatch")
			assert.Equal(t, tc.wantMessage, message, "message mismatch")
		})
	}
}

// TestHandleDeleteBucketReturns409WhenBucketHasObjects verifies that the
// gateway returns 409 BucketNotEmpty (not 204) when deleting a non-empty
// bucket via the public S3 API.
func TestHandleDeleteBucketReturns409WhenBucketHasObjects(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	ctx := context.Background()

	// Create a separate bucket for this test to avoid interfering with the
	// shared test-bucket that other tests use.
	bucketName := "delete-bucket-notempty"
	require.NoError(t, gateway.metadata.CreateBucket(ctx, bucketName, &metadata.BucketInfo{
		Name:      bucketName,
		CreatedAt: time.Now(),
		OwnerID:   "test-user",
		OwnerName: "test-user",
		Region:    "us-east-1",
		ACL:       "private",
	}))

	objMeta, storeMeta := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:      bucketName,
		Key:         "blocking-object.txt",
		Size:        5,
		ContentType: "text/plain",
		ETag:        "etag-notempty",
		StorageTier: common.TierHot,
		VersionID:   "v-1",
		Now:         time.Now(),
	})
	require.NoError(t, gateway.putStoredObjectWithMetadata(
		ctx,
		bucketName,
		"blocking-object.txt",
		strings.NewReader("hello"),
		5,
		common.TierHot,
		objMeta,
		storeMeta,
		"put object",
	))

	req := httptest.NewRequest(http.MethodDelete, "/"+bucketName, nil)
	withAuth(gateway, req)
	w := httptest.NewRecorder()

	err := gateway.bucketSvc.handleDeleteBucket(w, req, bucketName)
	require.Error(t, err, "expected error for non-empty bucket delete")
	require.True(t, errors.Is(err, metadata.ErrBucketNotEmpty),
		"expected ErrBucketNotEmpty, got %v", err)

	status, code, _ := gateway.classifyHandlerError(err)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "BucketNotEmpty", code)
	assert.Equal(t, http.StatusOK, w.Code, "handler must not have written a response")

	// The bucket must still exist after the rejected delete.
	_, getErr := gateway.metadata.GetBucket(ctx, bucketName)
	require.NoError(t, getErr, "bucket should still exist after rejected delete")
}

// TestHandleDeleteBucketReturns404WhenBucketMissing verifies that deleting a
// non-existent bucket surfaces as 404 NoSuchBucket rather than 204.
func TestHandleDeleteBucketReturns404WhenBucketMissing(t *testing.T) {
	gateway, _ := setupTestGateway(t)

	req := httptest.NewRequest(http.MethodDelete, "/missing-bucket-name", nil)
	withAuth(gateway, req)
	w := httptest.NewRecorder()

	err := gateway.bucketSvc.handleDeleteBucket(w, req, "missing-bucket-name")
	require.Error(t, err)
	require.True(t, errors.Is(err, metadata.ErrBucketNotFound),
		"expected ErrBucketNotFound, got %v", err)

	status, code, _ := gateway.classifyHandlerError(err)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "NoSuchBucket", code)
}

// TestHandleDeleteBucketSucceedsOnEmptyBucket verifies that an empty bucket
// can still be deleted via the gateway and returns no error.
func TestHandleDeleteBucketSucceedsOnEmptyBucket(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	ctx := context.Background()

	bucketName := "delete-bucket-empty"
	require.NoError(t, gateway.metadata.CreateBucket(ctx, bucketName, &metadata.BucketInfo{
		Name:      bucketName,
		CreatedAt: time.Now(),
		OwnerID:   "test-user",
		OwnerName: "test-user",
		Region:    "us-east-1",
		ACL:       "private",
	}))

	req := httptest.NewRequest(http.MethodDelete, "/"+bucketName, nil)
	withAuth(gateway, req)
	w := httptest.NewRecorder()

	require.NoError(t, gateway.bucketSvc.handleDeleteBucket(w, req, bucketName))
	assert.Equal(t, http.StatusNoContent, w.Code)

	_, getErr := gateway.metadata.GetBucket(ctx, bucketName)
	require.Error(t, getErr, "bucket should be gone after successful delete")
}

// TestHandleCreateBucketReturns409OnDuplicate verifies that creating a
// bucket which already exists surfaces as 409 BucketAlreadyExists.
func TestHandleCreateBucketReturns409OnDuplicate(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	ctx := context.Background()

	bucketName := "dup-bucket"
	require.NoError(t, gateway.metadata.CreateBucket(ctx, bucketName, &metadata.BucketInfo{
		Name:      bucketName,
		CreatedAt: time.Now(),
		OwnerID:   "test-user",
		OwnerName: "test-user",
		Region:    "us-east-1",
		ACL:       "private",
	}))

	req := httptest.NewRequest(http.MethodPut, "/"+bucketName, nil)
	withAuth(gateway, req)
	w := httptest.NewRecorder()

	err := gateway.bucketSvc.handleCreateBucket(w, req, bucketName)
	require.Error(t, err)
	require.True(t, errors.Is(err, metadata.ErrBucketAlreadyExists),
		"expected ErrBucketAlreadyExists, got %v", err)

	status, code, _ := gateway.classifyHandlerError(err)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "BucketAlreadyExists", code)
}

// TestStatusRecorderCapturesStatus verifies that statusRecorder correctly
// captures the status code and bytes written for access logging.
func TestStatusRecorderCapturesStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	sr.WriteHeader(http.StatusNotFound)
	assert.True(t, sr.written)
	assert.Equal(t, http.StatusNotFound, sr.status)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	n, err := sr.Write([]byte("error body"))
	require.NoError(t, err)
	assert.Equal(t, 10, n)
	assert.Equal(t, int64(10), sr.bytesWritten)
	assert.Equal(t, "error body", rec.Body.String())
}

// TestStatusRecorderWriteWithoutWriteHeader verifies that calling Write
// without WriteHeader defaults to 200 OK.
func TestStatusRecorderWriteWithoutWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	n, err := sr.Write([]byte("payload"))
	require.NoError(t, err)
	assert.Equal(t, 7, n)
	assert.True(t, sr.written)
	assert.Equal(t, http.StatusOK, sr.status)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestStatusRecorderOnlyWritesHeaderOnce verifies that multiple WriteHeader
// calls do not overwrite the recorded status and do not double-write to the
// underlying ResponseWriter.
func TestStatusRecorderOnlyWritesHeaderOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	sr.WriteHeader(http.StatusNotFound)
	sr.WriteHeader(http.StatusInternalServerError) // should be ignored

	assert.Equal(t, http.StatusNotFound, sr.status)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
