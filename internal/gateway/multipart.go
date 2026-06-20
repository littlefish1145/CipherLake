package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"nexus/internal/auth"
	"nexus/internal/common"
	"nexus/internal/events"
	"nexus/internal/metadata"
	"nexus/internal/taskqueue"
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

	userID := h.gateway.getUserID(r)
	if userID == "" {
		userID = "anonymous"
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	metadataMap := make(map[string]string)
	for k, values := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			metadataMap[strings.TrimPrefix(k, "x-amz-meta-")] = values[0]
		}
	}

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

	objMetadata := &metadata.ObjectMetadata{
		Key:            key,
		Bucket:         bucket,
		Size:           totalSize,
		ContentType:    upload.ContentType,
		ETag:           etag,
		UserMetadata:   upload.Metadata,
		StorageTier:    int(common.TierHot),
		CreatedAt:      time.Now(),
		ModifiedAt:     time.Now(),
		AccessCount:    0,
		LastAccessedAt: time.Now(),
		Encrypted:      upload.Encrypted,
		VersionID:      uuid.New().String(),
		IsLatest:       true,
		ObjectStatus:   "active",
	}

	if err := h.gateway.metadata.PutObject(r.Context(), bucket, key, objMetadata); err != nil {
		return fmt.Errorf("failed to store metadata: %w", err)
	}

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

	commonMeta := &common.ObjectMetadata{
		Key:          objMetadata.Key,
		Bucket:       objMetadata.Bucket,
		Size:         objMetadata.Size,
		ContentType:  objMetadata.ContentType,
		ETag:         objMetadata.ETag,
		UserMetadata: objMetadata.UserMetadata,
		StorageTier:  common.StorageTier(objMetadata.StorageTier),
		CreatedAt:    objMetadata.CreatedAt,
		ModifiedAt:   objMetadata.ModifiedAt,
		Encrypted:    objMetadata.Encrypted,
		Vectorized:   objMetadata.Vectorized,
		VersionID:    objMetadata.VersionID,
	}

	storageTier := common.StorageTier(objMetadata.StorageTier)
	if err := h.gateway.store.Put(r.Context(), bucket, key, pr, totalSize, storageTier, commonMeta); err != nil {
		return fmt.Errorf("failed to store merged object: %w", err)
	}

	go h.cleanupParts(uploadID)

	if err := h.gateway.metadata.DeleteUpload(r.Context(), bucket, key, uploadID); err != nil {
	}

	if h.gateway.tiering != nil {
		h.gateway.tiering.RecordAccess(r.Context(), bucket, key, "PUT", upload.UserID)
	}

	if h.gateway.vector != nil && h.gateway.config.Vector.Enabled {
		if r.Header.Get("X-Vectorize") != "false" {
			h.gateway.submitBackgroundTask(r.Context(), taskqueue.KindVectorize, taskqueue.VectorizePayload{
				Bucket:      bucket,
				Key:         key,
				ContentType: upload.ContentType,
				Metadata:    upload.Metadata,
				UserID:      upload.UserID,
			})
		}
	}

	if h.gateway.ftsIndex != nil && h.gateway.config.FTS.Enabled {
		if r.Header.Get("X-FTS-Index") != "false" {
			h.gateway.submitBackgroundTask(r.Context(), taskqueue.KindFTS, taskqueue.FTSPayload{
				Bucket:      bucket,
				Key:         key,
				ContentType: upload.ContentType,
				VersionID:   objMetadata.VersionID,
			})
		}
	}

	if h.gateway.pipeline != nil {
		h.gateway.submitBackgroundTask(r.Context(), taskqueue.KindPipeline, taskqueue.PipelinePayload{
			Bucket:      bucket,
			Key:         key,
			ContentType: upload.ContentType,
			Metadata:    upload.Metadata,
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

func (h *MultipartUploadHandler) cleanupParts(uploadID string) {
	uploadDir := h.getUploadDir(uploadID)
	filepath.Walk(uploadDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			os.Remove(path)
		}
		return nil
	})
	os.Remove(uploadDir)
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

	go h.cleanupParts(uploadID)

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
	buckets, err := h.gateway.metadata.ListBuckets(ctx)
	if err != nil {
		return fmt.Errorf("failed to list buckets: %w", err)
	}

	now := time.Now()
	for _, bucket := range buckets {
		uploads, err := h.gateway.metadata.ListUploads(ctx, bucket.Name)
		if err != nil {
			continue
		}

		for _, upload := range uploads {
			if now.After(upload.ExpiresAt) {
				if err := h.gateway.metadata.DeleteUpload(ctx, bucket.Name, upload.Key, upload.UploadID); err != nil {
				}
			}
		}
	}

	return nil
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
