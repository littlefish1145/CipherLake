package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"cipherlake/internal/auth"
	"cipherlake/internal/common"
	"cipherlake/internal/events"
	"cipherlake/internal/metadata"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type MultipartUploadHandler struct {
	gateway     *S3Gateway
	uploadsDir  string
	mu          sync.RWMutex
	partWriters map[string]int
}

func NewMultipartUploadHandler(gw *S3Gateway) *MultipartUploadHandler {
	uploadsDir := gw.config.Node.DataDir + "/uploads"
	os.MkdirAll(uploadsDir, 0755)

	return &MultipartUploadHandler{
		gateway:     gw,
		uploadsDir:  uploadsDir,
		partWriters: make(map[string]int),
	}
}

func (h *MultipartUploadHandler) getUploadDir(uploadID string) string {
	return filepath.Join(h.uploadsDir, uploadID)
}

func (h *MultipartUploadHandler) getPartPath(uploadID string, partNumber int) string {
	return filepath.Join(h.getUploadDir(uploadID), fmt.Sprintf("part-%d", partNumber))
}

type CreateMultipartUploadOutput struct {
	XMLName  xml.Name `xml:"CreateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type UploadPartOutput struct {
	ETag       string `xml:"ETag"`
	PartNumber int    `xml:"PartNumber"`
}

type CompleteMultipartUploadOutput struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
	Checksum string   `xml:"Checksum,omitempty"`
}

type ListPartsOutput struct {
	XMLName              xml.Name `xml:"ListPartsResult"`
	Bucket               string   `xml:"Bucket"`
	Key                  string   `xml:"Key"`
	UploadID             string   `xml:"UploadId"`
	StorageClass         string   `xml:"StorageClass"`
	MaxParts             int      `xml:"MaxParts"`
	IsTruncated          bool     `xml:"IsTruncated"`
	NextPartNumberMarker int      `xml:"NextPartNumberMarker,omitempty"`
	PartNumberMarker     int      `xml:"PartNumberMarker,omitempty"`
	Parts                []Part   `xml:"Parts"`
}

type Part struct {
	PartNumber   int       `xml:"PartNumber"`
	ETag         string    `xml:"ETag"`
	LastModified time.Time `xml:"LastModified"`
	Size         int64     `xml:"Size"`
	Checksum     string    `xml:"Checksum,omitempty"`
}

type ListUploadsOutput struct {
	XMLName            xml.Name `xml:"ListUploadsResult"`
	Bucket             string   `xml:"Bucket"`
	KeyMarker          string   `xml:"KeyMarker,omitempty"`
	UploadIDMarker     string   `xml:"UploadIDMarker,omitempty"`
	NextKeyMarker      string   `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string   `xml:"NextUploadIDMarker,omitempty"`
	MaxUploads         int      `xml:"MaxUploads"`
	IsTruncated        bool     `xml:"IsTruncated"`
	Uploads            []Upload `xml:"Upload"`
}

type Upload struct {
	Key       string    `xml:"Key"`
	UploadID  string    `xml:"UploadId"`
	Initiated time.Time `xml:"Initiated"`
}

func (h *MultipartUploadHandler) HandleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "POST" {
		return fmt.Errorf("method not allowed")
	}

	if _, err := h.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	userID := h.gateway.requestUserID(r)

	contentType := objectContentType(r.Header)

	metadataMap := parseUserMetadata(r.Header)

	upload := &metadata.MultipartUpload{
		UploadID:    uuid.New().String(),
		Bucket:      bucket,
		Key:         key,
		UserID:      userID,
		ContentType: contentType,
		Metadata:    metadataMap,
		Initiated:   time.Now(),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}

	if err := h.gateway.metadata.PutUpload(r.Context(), upload); err != nil {
		return fmt.Errorf("failed to create upload: %w", err)
	}

	uploadDir := h.getUploadDir(upload.UploadID)
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return fmt.Errorf("failed to create upload directory: %w", err)
	}

	w.Header().Set("x-amz-upload-id", upload.UploadID)

	output := CreateMultipartUploadOutput{
		Bucket:   bucket,
		Key:      key,
		UploadID: upload.UploadID,
	}

	return h.gateway.writeXML(w, http.StatusOK, output)
}

func (h *MultipartUploadHandler) HandleUploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "PUT" {
		return fmt.Errorf("method not allowed")
	}

	if _, err := h.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	uploadID := r.URL.Query().Get("uploadId")
	partNumberStr := r.URL.Query().Get("partNumber")

	if uploadID == "" {
		return fmt.Errorf("uploadId is required")
	}

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		return fmt.Errorf("invalid partNumber")
	}

	upload, err := h.gateway.metadata.GetUpload(r.Context(), bucket, key, uploadID)
	if err != nil {
		return fmt.Errorf("upload not found: %w", err)
	}

	if time.Now().After(upload.ExpiresAt) {
		return fmt.Errorf("upload has expired")
	}

	contentLength := r.ContentLength
	if contentLength < 0 {
		contentLength = 0
	}

	uploadDir := h.getUploadDir(uploadID)
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return fmt.Errorf("failed to create upload directory: %w", err)
	}

	partPath := h.getPartPath(uploadID, partNumber)
	tempPath := partPath + ".tmp"

	partFile, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create part file: %w", err)
	}
	defer partFile.Close()

	hasher := sha256.New()
	teeReader := io.TeeReader(r.Body, hasher)

	written, err := io.Copy(partFile, teeReader)
	if err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to write part data: %w", err)
	}

	if err := partFile.Sync(); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to sync part file: %w", err)
	}

	if err := os.Rename(tempPath, partPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to finalize part file: %w", err)
	}

	checksum := base64.StdEncoding.EncodeToString(hasher.Sum(nil))
	etag := fmt.Sprintf("%x", sha256.Sum256([]byte(uploadID+partNumberStr)))

	part := &metadata.UploadPart{
		PartNumber: partNumber,
		ETag:       etag,
		Size:       written,
		UploadedAt: time.Now(),
		Checksum:   checksum,
	}

	if err := h.gateway.metadata.AddPart(r.Context(), uploadID, part); err != nil {
		os.Remove(partPath)
		return fmt.Errorf("failed to add part: %w", err)
	}

	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("x-amz-checksum-sha256", checksum)
	w.WriteHeader(http.StatusOK)

	return nil
}

func (h *MultipartUploadHandler) HandleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "POST" {
		return fmt.Errorf("method not allowed")
	}

	if _, err := h.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		return fmt.Errorf("uploadId is required")
	}

	upload, err := h.gateway.metadata.GetUpload(r.Context(), bucket, key, uploadID)
	if err != nil {
		return fmt.Errorf("upload not found: %w", err)
	}

	if time.Now().After(upload.ExpiresAt) {
		return fmt.Errorf("upload has expired")
	}

	parts, err := h.gateway.metadata.GetParts(r.Context(), uploadID)
	if err != nil {
		return fmt.Errorf("failed to get parts: %w", err)
	}

	if len(parts) == 0 {
		return fmt.Errorf("no parts uploaded")
	}

	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	var totalSize int64
	for _, part := range parts {
		totalSize += part.Size
	}

	etag := uuid.New().String()

	commonMeta, objMetadata := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:       bucket,
		Key:          key,
		Size:         totalSize,
		ContentType:  upload.ContentType,
		ETag:         etag,
		UserMetadata: upload.Metadata,
		StorageTier:  common.TierHot,
		Encrypted:    upload.Encrypted,
	})

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		var bytesWritten int64
		for _, part := range parts {
			partPath := h.getPartPath(uploadID, part.PartNumber)
			partFile, err := os.Open(partPath)
			if err != nil {
				pw.CloseWithError(fmt.Errorf("failed to open part %d: %w", part.PartNumber, err))
				return
			}
			written, err := io.Copy(pw, partFile)
			partFile.Close()
			if err != nil {
				pw.CloseWithError(fmt.Errorf("failed to copy part %d: %w", part.PartNumber, err))
				return
			}
			bytesWritten += written
		}
		if bytesWritten != totalSize {
			pw.CloseWithError(fmt.Errorf("part merge size mismatch: expected %d, got %d", totalSize, bytesWritten))
			return
		}
		pw.Close()
	}()

	storageTier := common.StorageTier(objMetadata.StorageTier)
	if err := h.gateway.putStoredObjectWithMetadata(
		r.Context(),
		bucket,
		key,
		pr,
		totalSize,
		storageTier,
		commonMeta,
		objMetadata,
		"failed to store merged object",
	); err != nil {
		return err
	}

	// Synchronously clean up part files. The previous async `go cleanupParts`
	// could leak part files if the process exited before the goroutine
	// completed. The merged object is already persisted, so the parts are
	// no longer needed.
	if cleanupErr := h.cleanupParts(uploadID); cleanupErr != nil {
		zap.L().Warn("failed to clean up part files after complete-multipart",
			zap.String("upload_id", uploadID),
			zap.String("bucket", bucket),
			zap.String("key", key),
			zap.Error(cleanupErr))
	}

	if err := h.gateway.metadata.DeleteUpload(r.Context(), bucket, key, uploadID); err != nil {
		return fmt.Errorf("failed to finalize upload metadata: %w", err)
	}

	if h.gateway.tiering != nil {
		h.gateway.tiering.RecordAccess(r.Context(), bucket, key, "PUT", upload.UserID)
	}

	if h.gateway.writeTasks != nil {
		h.gateway.writeTasks.ScheduleObjectWriteTasks(r.Context(), objectWriteTaskRequest{
			Bucket:        bucket,
			Key:           key,
			ContentType:   upload.ContentType,
			Metadata:      upload.Metadata,
			UserID:        upload.UserID,
			VersionID:     objMetadata.VersionID,
			SkipVectorize: r.Header.Get("X-Vectorize") == "false",
			SkipFTS:       r.Header.Get("X-FTS-Index") == "false",
		})
	}

	if h.gateway.eventBus != nil {
		h.gateway.eventBus.Publish(r.Context(), &events.Event{
			EventType:    "s3:ObjectCreated:CompleteMultipartUpload",
			Bucket:       bucket,
			Key:          key,
			VersionID:    objMetadata.VersionID,
			ETag:         etag,
			Size:         totalSize,
			RequesterARN: upload.UserID,
		})
	}

	w.Header().Set("ETag", `"`+etag+`"`)

	output := CompleteMultipartUploadOutput{
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     etag,
	}

	return h.gateway.writeXML(w, http.StatusOK, output)
}

// cleanupParts removes the on-disk part files for an upload ID. It returns an
// aggregated error describing any failures encountered during removal so
// callers can log them instead of silently dropping them. The error is
// constructed via errors.Join so partial failures (some parts removed, others
// not) are still surfaced. A missing upload directory is treated as a no-op
// (nil error) so callers can invoke cleanupParts idempotently.
func (h *MultipartUploadHandler) cleanupParts(uploadID string) error {
	uploadDir := h.getUploadDir(uploadID)

	// If the upload directory doesn't exist, there's nothing to clean up.
	// This makes cleanupParts safe to call repeatedly (e.g. after a crash
	// where the directory was already removed).
	if _, err := os.Stat(uploadDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat upload dir %s: %w", uploadDir, err)
	}

	var errs error

	walkErr := filepath.Walk(uploadDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("walk error on %s: %w", path, err))
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			errs = errors.Join(errs, fmt.Errorf("failed to remove part file %s: %w", path, removeErr))
		}
		return nil
	})
	if walkErr != nil {
		errs = errors.Join(errs, walkErr)
	}

	if removeErr := os.Remove(uploadDir); removeErr != nil && !os.IsNotExist(removeErr) {
		errs = errors.Join(errs, fmt.Errorf("failed to remove upload dir %s: %w", uploadDir, removeErr))
	}

	return errs
}

func (h *MultipartUploadHandler) HandleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "DELETE" {
		return fmt.Errorf("method not allowed")
	}

	if _, err := h.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		return fmt.Errorf("uploadId is required")
	}

	// Synchronously clean up part files to avoid leaking them if the process
	// exits before an async goroutine completes.
	if cleanupErr := h.cleanupParts(uploadID); cleanupErr != nil {
		zap.L().Warn("failed to clean up part files after abort-multipart",
			zap.String("upload_id", uploadID),
			zap.String("bucket", bucket),
			zap.String("key", key),
			zap.Error(cleanupErr))
	}

	if err := h.gateway.metadata.DeleteUpload(r.Context(), bucket, key, uploadID); err != nil {
		return fmt.Errorf("failed to abort upload: %w", err)
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *MultipartUploadHandler) HandleListParts(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "GET" {
		return fmt.Errorf("method not allowed")
	}

	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		return fmt.Errorf("uploadId is required")
	}

	parts, err := h.gateway.metadata.GetParts(r.Context(), uploadID)
	if err != nil {
		return fmt.Errorf("failed to get parts: %w", err)
	}

	maxPartsStr := r.URL.Query().Get("max-parts")
	maxParts := 1000
	if maxPartsStr != "" {
		if m, err := strconv.Atoi(maxPartsStr); err == nil {
			maxParts = m
		}
	}

	partNumberMarkerStr := r.URL.Query().Get("part-number-marker")
	partNumberMarker := 0
	if partNumberMarkerStr != "" {
		if m, err := strconv.Atoi(partNumberMarkerStr); err == nil {
			partNumberMarker = m
		}
	}

	output := ListPartsOutput{
		Bucket:       bucket,
		Key:          key,
		UploadID:     uploadID,
		StorageClass: "STANDARD",
		MaxParts:     maxParts,
	}

	for _, part := range parts {
		if part.PartNumber <= partNumberMarker {
			continue
		}
		if len(output.Parts) >= maxParts {
			output.IsTruncated = true
			break
		}
		output.Parts = append(output.Parts, Part{
			PartNumber:   part.PartNumber,
			ETag:         `"` + part.ETag + `"`,
			LastModified: part.UploadedAt,
			Size:         part.Size,
			Checksum:     part.Checksum,
		})
	}

	return h.gateway.writeXML(w, http.StatusOK, output)
}

func (h *MultipartUploadHandler) HandleListUploads(w http.ResponseWriter, r *http.Request, bucket string) error {
	if r.Method != "GET" {
		return fmt.Errorf("method not allowed")
	}

	uploads, err := h.gateway.metadata.ListUploads(r.Context(), bucket)
	if err != nil {
		return fmt.Errorf("failed to list uploads: %w", err)
	}

	maxUploadsStr := r.URL.Query().Get("max-uploads")
	maxUploads := 1000
	if maxUploadsStr != "" {
		if m, err := strconv.Atoi(maxUploadsStr); err == nil {
			maxUploads = m
		}
	}

	output := ListUploadsOutput{
		Bucket:     bucket,
		MaxUploads: maxUploads,
	}

	for _, upload := range uploads {
		if len(output.Uploads) >= maxUploads {
			output.IsTruncated = true
			break
		}
		output.Uploads = append(output.Uploads, Upload{
			Key:       upload.Key,
			UploadID:  upload.UploadID,
			Initiated: upload.Initiated,
		})
	}

	return h.gateway.writeXML(w, http.StatusOK, output)
}

func (h *MultipartUploadHandler) CleanupExpiredUploads(ctx context.Context) error {
	uploads, err := h.gateway.metadata.ListExpiredUploads(ctx)
	if err != nil {
		return fmt.Errorf("failed to list expired uploads: %w", err)
	}

	var cleanupErr error
	for _, upload := range uploads {
		if err := h.gateway.metadata.DeleteUpload(ctx, upload.Bucket, upload.Key, upload.UploadID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete expired upload %s: %w", upload.UploadID, err))
			continue
		}
		if partErr := h.cleanupParts(upload.UploadID); partErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup parts for expired upload %s: %w", upload.UploadID, partErr))
		}
	}

	return cleanupErr
}

type CompleteUploadInput struct {
	Parts []CompletedPart `xml:"Part"`
}

type CompletedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

func (h *MultipartUploadHandler) ParseCompleteInput(r io.Reader) (*CompleteUploadInput, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	var input CompleteUploadInput
	if err := xml.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("failed to parse XML: %w", err)
	}

	return &input, nil
}
