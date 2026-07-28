package gateway

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cipherlake/internal/common"
	"cipherlake/internal/storage"
)

func TestHandleDeleteObjectReturnsErrorWhenStorageDeleteFails(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	ctx := context.Background()
	deleteErr := errors.New("storage delete failed")

	commonMeta, storeMeta := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:      "test-bucket",
		Key:         "delete-fail.txt",
		Size:        5,
		ContentType: "text/plain",
		ETag:        "etag",
		StorageTier: common.TierHot,
		VersionID:   "version",
		Now:         time.Now(),
	})
	if err := gateway.putStoredObjectWithMetadata(
		ctx,
		"test-bucket",
		"delete-fail.txt",
		bytes.NewReader([]byte("hello")),
		5,
		common.TierHot,
		commonMeta,
		storeMeta,
		"put object",
	); err != nil {
		t.Fatalf("put object: %v", err)
	}

	gateway.store.RegisterTier(common.TierHot, &deleteFailingBackend{BackendStorage: mustFileBackend(t), err: deleteErr}, 1<<30)

	req := httptest.NewRequest(http.MethodDelete, "/test-bucket/delete-fail.txt", nil)
	withAuth(gateway, req)
	w := httptest.NewRecorder()

	err := gateway.objectSvc.handleDeleteObject(w, req, "test-bucket", "delete-fail.txt")
	if !errors.Is(err, deleteErr) {
		t.Fatalf("handleDeleteObject() error = %v, want storage delete error", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want no response written", w.Code)
	}
	if _, err := gateway.metadata.GetObject(ctx, "test-bucket", "delete-fail.txt"); err != nil {
		t.Fatalf("metadata should remain after failed data delete: %v", err)
	}
}

func TestHandleDeleteObjectsReportsStorageDeleteFailurePerObject(t *testing.T) {
	gateway, _ := setupTestGateway(t)
	ctx := context.Background()
	deleteErr := errors.New("storage delete failed")

	commonMeta, storeMeta := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:      "test-bucket",
		Key:         "batch-delete-fail.txt",
		Size:        5,
		ContentType: "text/plain",
		ETag:        "etag",
		StorageTier: common.TierHot,
		VersionID:   "version",
		Now:         time.Now(),
	})
	if err := gateway.putStoredObjectWithMetadata(
		ctx,
		"test-bucket",
		"batch-delete-fail.txt",
		bytes.NewReader([]byte("hello")),
		5,
		common.TierHot,
		commonMeta,
		storeMeta,
		"put object",
	); err != nil {
		t.Fatalf("put object: %v", err)
	}

	gateway.store.RegisterTier(common.TierHot, &deleteFailingBackend{BackendStorage: mustFileBackend(t), err: deleteErr}, 1<<30)

	body := `<Delete><Object><Key>batch-delete-fail.txt</Key></Object></Delete>`
	req := httptest.NewRequest(http.MethodPost, "/test-bucket?delete", strings.NewReader(body))
	withAuth(gateway, req)
	w := httptest.NewRecorder()

	err := gateway.objectSvc.handleDeleteObjects(w, req, "test-bucket")
	if err != nil {
		t.Fatalf("handleDeleteObjects() error = %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var result DeleteObjectsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode delete result: %v", err)
	}
	if len(result.Deleted) != 0 {
		t.Fatalf("deleted = %+v, want none", result.Deleted)
	}
	if len(result.Error) != 1 {
		t.Fatalf("errors = %+v, want one", result.Error)
	}
	if result.Error[0].Key != "batch-delete-fail.txt" || result.Error[0].Code != "InternalError" {
		t.Fatalf("error = %+v, want InternalError for object", result.Error[0])
	}
	if _, err := gateway.metadata.GetObject(ctx, "test-bucket", "batch-delete-fail.txt"); err != nil {
		t.Fatalf("metadata should remain after failed data delete: %v", err)
	}
}

func TestHandleDeleteObjectsQuietStillReportsErrors(t *testing.T) {
	gateway, _ := setupTestGateway(t)

	body := `<Delete><Quiet>true</Quiet><Object><Key>missing.txt</Key></Object></Delete>`
	req := httptest.NewRequest(http.MethodPost, "/test-bucket?delete", strings.NewReader(body))
	withAuth(gateway, req)
	w := httptest.NewRecorder()

	err := gateway.objectSvc.handleDeleteObjects(w, req, "test-bucket")
	if err != nil {
		t.Fatalf("handleDeleteObjects() error = %v", err)
	}

	var result DeleteObjectsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode delete result: %v", err)
	}
	if len(result.Deleted) != 0 {
		t.Fatalf("deleted = %+v, want none", result.Deleted)
	}
	if len(result.Error) != 1 {
		t.Fatalf("errors = %+v, want one", result.Error)
	}
	if result.Error[0].Code != "NoSuchKey" {
		t.Fatalf("error code = %q, want NoSuchKey", result.Error[0].Code)
	}
}

type deleteFailingBackend struct {
	storage.BackendStorage
	err error
}

func (b *deleteFailingBackend) Delete(ctx context.Context, path string) error {
	return b.err
}

func mustFileBackend(t *testing.T) storage.BackendStorage {
	t.Helper()

	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileBackend() error = %v", err)
	}
	return backend
}
