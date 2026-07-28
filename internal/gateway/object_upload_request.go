package gateway

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

const defaultObjectContentType = "application/octet-stream"

type uploadTooLargeError struct {
	size int64
	max  int64
}

func (e *uploadTooLargeError) Error() string {
	return fmt.Sprintf("object size %d exceeds maximum allowed size %d", e.size, e.max)
}

func objectContentType(header http.Header) string {
	contentType := header.Get("Content-Type")
	if contentType == "" {
		return defaultObjectContentType
	}
	return contentType
}

func (g *S3Gateway) parseObjectContentType(header http.Header) (string, error) {
	contentType := objectContentType(header)
	if !g.validateContentType(contentType) {
		return "", fmt.Errorf("unsupported content type: %s", contentType)
	}
	return contentType, nil
}

func (g *S3Gateway) requestUserID(r *http.Request) string {
	userID := g.getUserID(r)
	if userID == "" {
		return "anonymous"
	}
	return userID
}

func (g *S3Gateway) maxUploadBytes() int64 {
	if g == nil || g.config == nil {
		return 0
	}
	return g.config.Performance.MaxUploadBytes
}

func (g *S3Gateway) resolveUploadContentLength(r *http.Request) (int64, error) {
	maxUploadBytes := g.maxUploadBytes()
	contentLength := r.ContentLength

	if contentLength >= 0 {
		if maxUploadBytes > 0 && contentLength > maxUploadBytes {
			return contentLength, &uploadTooLargeError{size: contentLength, max: maxUploadBytes}
		}
		return contentLength, nil
	}

	var reader io.Reader = r.Body
	if maxUploadBytes > 0 {
		reader = io.LimitReader(r.Body, maxUploadBytes+1)
	}
	bodyBytes, err := io.ReadAll(reader)
	if err != nil {
		return 0, fmt.Errorf("failed to read request body: %w", err)
	}

	contentLength = int64(len(bodyBytes))
	if maxUploadBytes > 0 && contentLength > maxUploadBytes {
		return contentLength, &uploadTooLargeError{size: contentLength, max: maxUploadBytes}
	}

	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return contentLength, nil
}
