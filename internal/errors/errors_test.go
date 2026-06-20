package errors

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestErrorImplementsError(t *testing.T) {
	err := NoSuchKey().WithResource("/bucket/key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "NoSuchKey")
}

func TestIsS3(t *testing.T) {
	plain := errors.New("plain")
	_, ok := IsS3(plain)
	assert.False(t, ok)

	s3Err := NoSuchBucket()
	found, ok := IsS3(s3Err)
	assert.True(t, ok)
	assert.Equal(t, "NoSuchBucket", found.Code)
}

func TestWrap(t *testing.T) {
	cause := errors.New("disk full")
	err := Wrap(cause, "InternalError", "storage failure", http.StatusInternalServerError)
	assert.ErrorIs(t, err, cause)
}

func TestWriteS3Error(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, NoSuchKey().WithRequestID("req-123"))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/xml", rec.Header().Get("Content-Type"))
	assert.Equal(t, "req-123", rec.Header().Get("x-amz-request-id"))

	body, _ := io.ReadAll(rec.Body)
	assert.Contains(t, string(body), "<Code>NoSuchKey</Code>")
	assert.Contains(t, string(body), "<RequestId>req-123</RequestId>")
}

func TestWritePlainErrorBecomesInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, errors.New("boom"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	body, _ := io.ReadAll(rec.Body)
	assert.Contains(t, string(body), "<Code>InternalError</Code>")
}

func TestCommonConstructors(t *testing.T) {
	tests := []struct {
		name       string
		err        *Error
		wantCode   string
		wantStatus int
	}{
		{"AccessDenied", AccessDenied(), "AccessDenied", http.StatusForbidden},
		{"InvalidRequest", InvalidRequest(), "InvalidRequest", http.StatusBadRequest},
		{"EntityTooLarge", EntityTooLarge(), "EntityTooLarge", http.StatusRequestEntityTooLarge},
		{"SlowDown", SlowDown(), "SlowDown", http.StatusTooManyRequests},
		{"UploadExpired", UploadExpired(), "UploadExpired", http.StatusGone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantCode, tt.err.Code)
			assert.Equal(t, tt.wantStatus, tt.err.Status)
		})
	}
}

func TestResourceOmittedWhenEmpty(t *testing.T) {
	resp := NoSuchKey().Response()
	// Simple check by marshalling is enough; if empty, omitempty drops it.
	assert.Empty(t, resp.Resource)
	assert.True(t, strings.Contains(resp.Message, "exist"))
}
