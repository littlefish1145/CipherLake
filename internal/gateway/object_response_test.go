package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cipherlake/internal/common"
	"cipherlake/internal/metadata"
)

func TestWriteObjectResponseHeadersForGet(t *testing.T) {
	modifiedAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	objMeta := &metadata.ObjectMetadata{
		ContentType:  "text/plain",
		ETag:         "etag",
		ModifiedAt:   modifiedAt,
		UserMetadata: map[string]string{"foo": "bar"},
		Encrypted:    true,
		Checksum:     "checksum",
		ChecksumType: "sha256",
	}
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	w := httptest.NewRecorder()

	writeObjectResponseHeaders(w, req, objMeta, objectResponseHeaderOptions{
		ContentLength: 5,
		ETag:          `"etag"`,
		AcceptRanges:  true,
		CacheControl:  "max-age=3600",
		CacheStatus:   "HIT",
		ContentRange:  "bytes 0-4/10",
	})

	header := w.Header()
	if got := header.Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
	if got := header.Get("Content-Length"); got != "5" {
		t.Fatalf("Content-Length = %q, want 5", got)
	}
	if got := header.Get("ETag"); got != `"etag"` {
		t.Fatalf("ETag = %q, want quoted etag", got)
	}
	if got := header.Get("Last-Modified"); got != modifiedAt.Format(http.TimeFormat) {
		t.Fatalf("Last-Modified = %q, want %q", got, modifiedAt.Format(http.TimeFormat))
	}
	if got := header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}
	if got := header.Get("Cache-Control"); got != "max-age=3600" {
		t.Fatalf("Cache-Control = %q, want max-age=3600", got)
	}
	if got := header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", got)
	}
	if got := header.Get("Content-Range"); got != "bytes 0-4/10" {
		t.Fatalf("Content-Range = %q, want bytes 0-4/10", got)
	}
	if got := header.Get("X-Amz-Meta-Foo"); got != "bar" {
		t.Fatalf("X-Amz-Meta-Foo = %q, want bar", got)
	}
	if got := header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("encryption header = %q, want AES256", got)
	}
	if got := header.Get("x-amz-checksum-sha256"); got != "checksum" {
		t.Fatalf("checksum header = %q, want checksum", got)
	}
}

func TestWriteObjectResponseHeadersForHeadWithSSEC(t *testing.T) {
	objMeta := &metadata.ObjectMetadata{
		ContentType:  "application/octet-stream",
		ETag:         "etag",
		Size:         42,
		ModifiedAt:   time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		StorageTier:  int(common.TierWarm),
		UserMetadata: map[string]string{},
		SSECUsed:     true,
	}
	req := httptest.NewRequest(http.MethodHead, "/bucket/key", nil)
	req.Header.Set("x-amz-server-side-encryption-customer-key-MD5", "key-md5")
	w := httptest.NewRecorder()

	writeObjectResponseHeaders(w, req, objMeta, objectResponseHeaderOptions{
		ContentLength:       objMeta.Size,
		IncludeStorageClass: true,
	})

	header := w.Header()
	if got := header.Get("X-Amz-Storage-Class"); got != "WARM" {
		t.Fatalf("X-Amz-Storage-Class = %q, want WARM", got)
	}
	if got := header.Get("x-amz-server-side-encryption-customer-algorithm"); got != "AES256" {
		t.Fatalf("SSE-C algorithm = %q, want AES256", got)
	}
	if got := header.Get("x-amz-server-side-encryption-customer-key-MD5"); got != "key-md5" {
		t.Fatalf("SSE-C key MD5 = %q, want key-md5", got)
	}
	if got := header.Get("x-amz-server-side-encryption"); got != "" {
		t.Fatalf("SSE header = %q, want empty for SSE-C", got)
	}
}
