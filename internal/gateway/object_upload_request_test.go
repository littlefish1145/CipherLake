package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"cipherlake/internal/config"
)

func TestObjectContentTypeDefaultsToOctetStream(t *testing.T) {
	got := objectContentType(http.Header{})
	if got != defaultObjectContentType {
		t.Fatalf("content type = %q, want %q", got, defaultObjectContentType)
	}
}

func TestResolveUploadContentLengthRejectsOversizedKnownLength(t *testing.T) {
	gateway := &S3Gateway{config: &config.Config{}}
	gateway.config.Performance.MaxUploadBytes = 3

	req := &http.Request{
		ContentLength: 4,
		Body:          io.NopCloser(strings.NewReader("four")),
	}

	_, err := gateway.resolveUploadContentLength(req)
	if _, ok := err.(*uploadTooLargeError); !ok {
		t.Fatalf("error = %v, want uploadTooLargeError", err)
	}
}

func TestResolveUploadContentLengthRejectsOversizedUnknownLength(t *testing.T) {
	gateway := &S3Gateway{config: &config.Config{}}
	gateway.config.Performance.MaxUploadBytes = 3

	req := &http.Request{
		ContentLength: -1,
		Body:          io.NopCloser(strings.NewReader("four")),
	}

	_, err := gateway.resolveUploadContentLength(req)
	if _, ok := err.(*uploadTooLargeError); !ok {
		t.Fatalf("error = %v, want uploadTooLargeError", err)
	}
}

func TestResolveUploadContentLengthRewindsUnknownLengthBody(t *testing.T) {
	gateway := &S3Gateway{config: &config.Config{}}
	gateway.config.Performance.MaxUploadBytes = 10

	req := &http.Request{
		ContentLength: -1,
		Body:          io.NopCloser(strings.NewReader("hello")),
	}

	got, err := gateway.resolveUploadContentLength(req)
	if err != nil {
		t.Fatalf("resolve content length: %v", err)
	}
	if got != 5 {
		t.Fatalf("content length = %d, want 5", got)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read rewound body: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("rewound body = %q, want hello", string(body))
	}
}
