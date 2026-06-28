package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"hash/crc32"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/internal/metadata"
	"nexus/internal/taskqueue"
)

func (g *S3Gateway) writeError(w http.ResponseWriter, status int, code, message string) {
	requestID := uuid.New().String()
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("Content-Type", "application/xml")

	errorResp := ErrorResponse{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	}

	g.writeXML(w, status, errorResp)
}

func (g *S3Gateway) writeXML(w http.ResponseWriter, status int, data interface{}) error {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)

	encoder := xml.NewEncoder(w)
	encoder.Indent("", "  ")
	return encoder.Encode(data)
}

func (g *S3Gateway) writeJSON(w http.ResponseWriter, status int, data interface{}) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	return json.NewEncoder(w).Encode(data)
}

func (g *S3Gateway) submitBackgroundTask(ctx context.Context, kind string, payload any) {
	if g.taskQueue == nil {
		return
	}

	task, err := taskqueue.NewTask(kind, payload,
		taskqueue.WithRetry(g.config.TaskQueue.DefaultRetry),
		taskqueue.WithDeadlineAfter(5*time.Minute),
	)
	if err != nil {
		zap.L().Error("failed to create background task", zap.String("kind", kind), zap.Error(err))
		return
	}

	if err := g.taskQueue.Submit(ctx, task); err != nil {
		zap.L().Error("failed to submit background task",
			zap.String("kind", kind),
			zap.String("task_id", task.ID),
			zap.Error(err))
	}
}

func setChecksumResponseHeaders(w http.ResponseWriter, objMeta *metadata.ObjectMetadata, r *http.Request) {
	checksumMode := r.Header.Get("x-amz-checksum-mode")
	if checksumMode != "ENABLED" && (objMeta.Checksum == "" || objMeta.ChecksumType == "") {
		return
	}
	switch objMeta.ChecksumType {
	case "crc32c":
		w.Header().Set("x-amz-checksum-crc32c", objMeta.Checksum)
	case "sha256":
		w.Header().Set("x-amz-checksum-sha256", objMeta.Checksum)
	case "md5":
		w.Header().Set("x-amz-checksum-md5", objMeta.Checksum)
	}
}

func computeCRC32C(data []byte) string {
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
