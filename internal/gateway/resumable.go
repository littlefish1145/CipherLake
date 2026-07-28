package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cipherlake/internal/auth"
	"cipherlake/internal/common"
	"cipherlake/internal/events"
	"cipherlake/internal/metadata"
	"cipherlake/internal/observability"
	"github.com/google/uuid"

	"github.com/prometheus/client_golang/prometheus"
)

// Resumable upload metrics
var (
	resumableSessionActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "cipherlake",
		Name:      "resumable_session_active",
		Help:      "Current number of active resumable upload sessions",
	})
	resumableSessionCompleted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "cipherlake",
		Name:      "resumable_session_completed_total",
		Help:      "Total number of completed resumable upload sessions",
	})
	resumableSessionExpired = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "cipherlake",
		Name:      "resumable_session_expired_total",
		Help:      "Total number of expired resumable upload sessions",
	})
)

func init() {
	prometheus.MustRegister(resumableSessionActive, resumableSessionCompleted, resumableSessionExpired)
}

// ResumableUploadHandler handles tus-style resumable uploads.
//
// Coexistence with S3 multipart uploads:
// When the X-CipherLake-Resumable: 1 header is present on a POST request with ?resumable,
// the resumable upload flow is used. Otherwise, standard S3 multipart upload flow applies.
// Both flows can coexist for the same bucket because they use separate metadata buckets
// in BoltDB (resumable_uploads vs uploads) and separate temp file directories
// (data/uploads/{uploadId}.tmp vs data/uploads/{uploadId}/part-*).
// The resumable flow uses PATCH to append data and supports offset verification,
// while S3 multipart uses PUT with part numbers. Clients choose the flow via
// the X-CipherLake-Resumable header or the ?resumable query parameter.
type ResumableUploadHandler struct {
	gateway    *S3Gateway
	uploadsDir string
}

// NewResumableUploadHandler creates a new handler for resumable uploads.
func NewResumableUploadHandler(gw *S3Gateway) *ResumableUploadHandler {
	uploadsDir := gw.config.Resumable.UploadDir
	if uploadsDir == "" {
		uploadsDir = gw.config.Node.DataDir + "/uploads"
	}
	os.MkdirAll(uploadsDir, 0755)

	return &ResumableUploadHandler{
		gateway:    gw,
		uploadsDir: uploadsDir,
	}
}

func (h *ResumableUploadHandler) getTempFilePath(uploadID string) string {
	return filepath.Join(h.uploadsDir, uploadID+".tmp")
}

// HandleCreateSession creates a new resumable upload session.
// POST /{bucket}/{key}?resumable
func (h *ResumableUploadHandler) HandleCreateSession(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if _, err := h.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		h.gateway.writeError(w, http.StatusUnauthorized, "AccessDenied", err.Error())
		return nil
	}

	contentType := objectContentType(r.Header)

	metadataMap := parseUserMetadata(r.Header)

	var totalSize int64 = -1
	uploadLengthStr := r.Header.Get("Upload-Length")
	if uploadLengthStr != "" {
		if size, err := strconv.ParseInt(uploadLengthStr, 10, 64); err == nil {
			totalSize = size
		}
	}

	defaultExpiry := 24 * time.Hour
	if h.gateway.config.Resumable.DefaultExpiry != "" {
		if d, err := time.ParseDuration(h.gateway.config.Resumable.DefaultExpiry); err == nil {
			defaultExpiry = d
		}
	}

	uploadID := uuid.New().String()

	session := &metadata.ResumableSession{
		UploadID:    uploadID,
		Bucket:      bucket,
		Key:         key,
		Offset:      0,
		TotalSize:   totalSize,
		ContentType: contentType,
		Metadata:    metadataMap,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(defaultExpiry),
		Checksum:    "",
		Finalized:   false,
	}

	if err := h.gateway.metadata.PutResumableSession(r.Context(), session); err != nil {
		return fmt.Errorf("failed to create resumable session: %w", err)
	}

	tempFilePath := h.getTempFilePath(uploadID)
	tempFile, err := os.Create(tempFilePath)
	if err != nil {
		h.gateway.metadata.DeleteResumableSession(r.Context(), uploadID)
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tempFile.Close()

	resumableSessionActive.Inc()

	w.Header().Set("Location", "/"+bucket+"/"+key+"?uploadId="+uploadID)
	w.Header().Set("Upload-Offset", "0")
	w.Header().Set("X-CipherLake-Upload-Id", uploadID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	resp := map[string]string{
		"upload_id": uploadID,
		"location":  "/" + bucket + "/" + key + "?uploadId=" + uploadID,
	}
	json.NewEncoder(w).Encode(resp)

	return nil
}

// HandlePatch appends data to an existing resumable upload session.
// PATCH /{bucket}/{key}?uploadId=...
func (h *ResumableUploadHandler) HandlePatch(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if _, err := h.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		h.gateway.writeError(w, http.StatusUnauthorized, "AccessDenied", err.Error())
		return nil
	}

	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "uploadId is required")
		return nil
	}

	session, err := h.gateway.metadata.GetResumableSession(r.Context(), uploadID)
	if err != nil {
		h.gateway.writeError(w, http.StatusNotFound, "NoSuchUpload", "Upload session not found")
		return nil
	}

	if session.Bucket != bucket || session.Key != key {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "Bucket/key mismatch")
		return nil
	}

	if time.Now().After(session.ExpiresAt) {
		h.gateway.writeError(w, http.StatusGone, "UploadExpired", "Upload session has expired")
		return nil
	}

	if session.Finalized {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "Upload session already finalized")
		return nil
	}

	// Verify Upload-Offset header matches current offset
	offsetStr := r.Header.Get("Upload-Offset")
	if offsetStr == "" {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "Upload-Offset header is required")
		return nil
	}
	clientOffset, err := strconv.ParseInt(offsetStr, 10, 64)
	if err != nil {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid Upload-Offset header")
		return nil
	}
	if clientOffset != session.Offset {
		h.gateway.writeError(w, http.StatusConflict, "OffsetMismatch",
			fmt.Sprintf("Offset mismatch: client sent %d, server has %d", clientOffset, session.Offset))
		return nil
	}

	// Parse Upload-Checksum header if present (format: sha256 <base64>)
	var providedChecksum string
	checksumHeader := r.Header.Get("Upload-Checksum")
	if checksumHeader != "" {
		parts := strings.SplitN(checksumHeader, " ", 2)
		if len(parts) == 2 && strings.ToLower(parts[0]) == "sha256" {
			providedChecksum = parts[1]
		}
	}

	// Append data to temp file
	tempFilePath := h.getTempFilePath(uploadID)
	tempFile, err := os.OpenFile(tempFilePath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open temp file: %w", err)
	}
	tempFileClosed := false
	closeTempFile := func() error {
		if tempFileClosed {
			return nil
		}
		tempFileClosed = true
		return tempFile.Close()
	}
	defer closeTempFile()

	hasher := sha256.New()
	teeReader := io.TeeReader(r.Body, hasher)

	written, err := io.Copy(tempFile, teeReader)
	if err != nil {
		return fmt.Errorf("failed to write data: %w", err)
	}

	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	newOffset := session.Offset + written

	// Compute cumulative checksum: hash of all uploaded data so far
	// We read the full temp file to compute cumulative hash
	computedChecksum := ""
	if providedChecksum != "" || newOffset > 0 {
		checksum, err := h.computeFileChecksum(tempFilePath)
		if err == nil {
			computedChecksum = checksum
		}
	}

	// Verify checksum if provided
	if providedChecksum != "" && computedChecksum != providedChecksum {
		// Rollback: truncate file back to original offset
		_ = closeTempFile()
		if err := os.Truncate(tempFilePath, session.Offset); err != nil {
			// Log but don't fail the rollback
		}
		h.gateway.writeError(w, http.StatusBadRequest, "BadDigest",
			fmt.Sprintf("Checksum mismatch: expected %s, got %s", providedChecksum, computedChecksum))
		return nil
	}

	// Update session metadata
	session.Offset = newOffset
	if computedChecksum != "" {
		session.Checksum = computedChecksum
	}

	if err := h.gateway.metadata.PutResumableSession(r.Context(), session); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	// Check if finalize is requested
	if r.Header.Get("X-CipherLake-Finalize") == "1" {
		if err := closeTempFile(); err != nil {
			return fmt.Errorf("failed to close temp file before finalize: %w", err)
		}
		return h.HandleFinalize(w, r, bucket, key)
	}

	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	if computedChecksum != "" {
		w.Header().Set("Upload-Checksum", "sha256 "+computedChecksum)
	}
	w.Header().Set("X-CipherLake-Upload-Id", uploadID)
	w.WriteHeader(http.StatusNoContent)

	return nil
}

// HandleHead returns the current state of a resumable upload session.
// HEAD /{bucket}/{key}?uploadId=...
func (h *ResumableUploadHandler) HandleHead(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if _, err := h.gateway.requireIdentity(r, auth.ActionRead, bucket, key); err != nil {
		h.gateway.writeError(w, http.StatusUnauthorized, "AccessDenied", err.Error())
		return nil
	}

	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "uploadId is required")
		return nil
	}

	session, err := h.gateway.metadata.GetResumableSession(r.Context(), uploadID)
	if err != nil {
		h.gateway.writeError(w, http.StatusNotFound, "NoSuchUpload", "Upload session not found")
		return nil
	}

	if session.Bucket != bucket || session.Key != key {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "Bucket/key mismatch")
		return nil
	}

	if time.Now().After(session.ExpiresAt) {
		h.gateway.writeError(w, http.StatusGone, "UploadExpired", "Upload session has expired")
		return nil
	}

	w.Header().Set("Upload-Offset", strconv.FormatInt(session.Offset, 10))
	if session.TotalSize > 0 {
		w.Header().Set("Upload-Length", strconv.FormatInt(session.TotalSize, 10))
	}
	if session.Checksum != "" {
		w.Header().Set("Upload-Checksum", "sha256 "+session.Checksum)
	}
	w.Header().Set("X-CipherLake-Upload-Id", uploadID)
	w.WriteHeader(http.StatusNoContent)

	return nil
}

// HandleFinalize completes a resumable upload session.
// Triggered by X-CipherLake-Finalize: 1 header on a PATCH request,
// or by a separate POST with ?uploadId=...&finalize=1
func (h *ResumableUploadHandler) HandleFinalize(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if _, err := h.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		h.gateway.writeError(w, http.StatusUnauthorized, "AccessDenied", err.Error())
		return nil
	}

	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "uploadId is required")
		return nil
	}

	session, err := h.gateway.metadata.GetResumableSession(r.Context(), uploadID)
	if err != nil {
		h.gateway.writeError(w, http.StatusNotFound, "NoSuchUpload", "Upload session not found")
		return nil
	}

	if session.Bucket != bucket || session.Key != key {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "Bucket/key mismatch")
		return nil
	}

	if time.Now().After(session.ExpiresAt) {
		h.gateway.writeError(w, http.StatusGone, "UploadExpired", "Upload session has expired")
		return nil
	}

	if session.Finalized {
		h.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "Upload session already finalized")
		return nil
	}

	tempFilePath := h.getTempFilePath(uploadID)

	// Read temp file for storage
	tempFile, err := os.Open(tempFilePath)
	if err != nil {
		return fmt.Errorf("failed to open temp file: %w", err)
	}
	tempFileClosed := false
	closeTempFile := func() error {
		if tempFileClosed {
			return nil
		}
		tempFileClosed = true
		return tempFile.Close()
	}
	defer closeTempFile()

	fileInfo, err := tempFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat temp file: %w", err)
	}
	contentLength := fileInfo.Size()

	var dataReader io.Reader = tempFile
	var encryptedDEKMetadata []byte
	var encrypted bool
	var actualStorageSize int64 = contentLength

	userID := h.gateway.requestUserID(r)

	// Trigger encryption if enabled
	if h.gateway.cryptoCoordinator != nil && h.gateway.config != nil && h.gateway.config.CryptoServices.Enabled {
		encryptedReader, _, encMetadata, ciphertextSize, err := h.gateway.cryptoCoordinator.EncryptOperation(
			r.Context(), userID, bucket, key, tempFile, contentLength,
		)
		if err != nil {
			return fmt.Errorf("encryption failed: %w", err)
		}
		dataReader = encryptedReader
		encryptedDEKMetadata = encMetadata
		encrypted = true
		actualStorageSize = ciphertextSize
	}

	etag := uuid.New().String()
	versionID := uuid.New().String()

	objMetadata, meta := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:       bucket,
		Key:          key,
		Size:         contentLength,
		ContentType:  session.ContentType,
		ETag:         etag,
		UserMetadata: session.Metadata,
		StorageTier:  common.TierHot,
		Encrypted:    encrypted,
		VersionID:    versionID,
		EncryptedDEK: encryptedDEKMetadata,
	})

	storageTier := common.TierHot

	// Compute final checksum
	finalChecksum := session.Checksum
	if finalChecksum == "" {
		if cs, err := h.computeFileChecksum(tempFilePath); err == nil {
			finalChecksum = cs
		}
	}

	meta.Checksum = finalChecksum
	meta.ChecksumType = "sha256"

	if err := h.gateway.putStoredObjectWithMetadata(
		r.Context(),
		bucket,
		key,
		dataReader,
		actualStorageSize,
		storageTier,
		objMetadata,
		meta,
		"failed to store object",
	); err != nil {
		return err
	}

	// Mark session as finalized
	session.Finalized = true
	h.gateway.metadata.PutResumableSession(r.Context(), session)

	// Close temp file explicitly before cleanup so Windows can delete it.
	if err := closeTempFile(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Clean up session and temp file
	h.cleanupSession(r.Context(), uploadID, tempFilePath)

	// Trigger tiering
	if h.gateway.tiering != nil {
		h.gateway.tiering.RecordAccess(r.Context(), bucket, key, "PUT", userID)
	}

	if h.gateway.writeTasks != nil {
		h.gateway.writeTasks.ScheduleObjectWriteTasks(r.Context(), objectWriteTaskRequest{
			Bucket:        bucket,
			Key:           key,
			ContentType:   session.ContentType,
			Metadata:      session.Metadata,
			UserID:        userID,
			VersionID:     versionID,
			SkipVectorize: r.Header.Get("X-Vectorize") == "false",
			SkipFTS:       r.Header.Get("X-FTS-Index") == "false",
		})
	}

	// Publish event
	if h.gateway.eventBus != nil {
		h.gateway.eventBus.Publish(r.Context(), &events.Event{
			EventType:    "s3:ObjectCreated:ResumableUpload",
			Bucket:       bucket,
			Key:          key,
			VersionID:    versionID,
			ETag:         etag,
			Size:         contentLength,
			RequesterARN: userID,
			SourceIP:     extractIP(r),
		})
	}

	// Record metrics
	resumableSessionCompleted.Inc()
	resumableSessionActive.Dec()
	if h.gateway.metrics != nil {
		h.gateway.metrics.RecordPutObject(bucket, "success")
	}

	// Audit log
	h.gateway.auditLog(r, "RESUMABLE_FINALIZE", bucket, key, userID, "success", map[string]interface{}{
		"size":      contentLength,
		"upload_id": uploadID,
		"encrypted": encrypted,
	})

	// Return standard PutObject response
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("x-amz-version-id", versionID)
	if encrypted {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}
	w.Header().Set("X-CipherLake-Upload-Id", uploadID)
	w.WriteHeader(http.StatusOK)

	return nil
}

func (h *ResumableUploadHandler) cleanupSession(ctx context.Context, uploadID, tempFilePath string) {
	if tempFilePath != "" {
		os.Remove(tempFilePath)
	}
	h.gateway.metadata.DeleteResumableSession(ctx, uploadID)
}

func (h *ResumableUploadHandler) computeFileChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(hasher.Sum(nil)), nil
}

// RecordSessionExpired increments the expired session metric.
func RecordSessionExpired() {
	resumableSessionExpired.Inc()
	resumableSessionActive.Dec()
}

// GetActiveSessionGauge returns the active session gauge for external use.
func GetActiveSessionGauge() prometheus.Gauge {
	return resumableSessionActive
}

// Ensure observability.MetricsRegistry is available (used in s3.go).
var _ = observability.MetricsRegistry{}
