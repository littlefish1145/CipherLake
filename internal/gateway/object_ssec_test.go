package gateway

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cipherlake/internal/metadata"
	"cipherlake/internal/services"
)

func TestValidateObjectSSECKeyReturnsValidatedKey(t *testing.T) {
	clientKey := make([]byte, 32)
	if _, err := rand.Read(clientKey); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	req := newSSECTestRequest(t, clientKey)
	objMeta := &metadata.ObjectMetadata{
		SSECUsed:      true,
		SSECKeySHA256: services.ComputeSSECKeySHA256(clientKey),
	}
	service := &ObjectService{gateway: &S3Gateway{}}
	w := httptest.NewRecorder()

	gotKey, ok := service.validateObjectSSECKey(w, req, objMeta, "Invalid SSE-C customer key headers")
	if !ok {
		t.Fatalf("validate key returned ok=false, response=%d body=%s", w.Code, w.Body.String())
	}
	if string(gotKey) != string(clientKey) {
		t.Fatalf("validated key does not match input key")
	}
}

func TestValidateObjectSSECKeyRejectsMissingKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	objMeta := &metadata.ObjectMetadata{SSECUsed: true}
	service := &ObjectService{gateway: &S3Gateway{}}
	w := httptest.NewRecorder()

	_, ok := service.validateObjectSSECKey(w, req, objMeta, "Invalid SSE-C customer key headers")
	if ok {
		t.Fatalf("validate key returned ok=true")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if body := w.Body.String(); !containsAll(body, "AccessDenied", missingSSECKeyMessage) {
		t.Fatalf("body = %q, want AccessDenied missing key message", body)
	}
}

func TestValidateObjectSSECKeyRejectsMismatchedKeyAndZerosIt(t *testing.T) {
	clientKey := make([]byte, 32)
	if _, err := rand.Read(clientKey); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	req := newSSECTestRequest(t, clientKey)
	objMeta := &metadata.ObjectMetadata{
		SSECUsed:      true,
		SSECKeySHA256: services.ComputeSSECKeySHA256([]byte("wrong key material")),
	}
	service := &ObjectService{gateway: &S3Gateway{}}
	w := httptest.NewRecorder()

	gotKey, ok := service.validateObjectSSECKey(w, req, objMeta, "Invalid SSE-C customer key headers")
	if ok {
		t.Fatalf("validate key returned ok=true")
	}
	if gotKey != nil {
		t.Fatalf("validated key = %v, want nil", gotKey)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if body := w.Body.String(); !containsAll(body, "AccessDenied", mismatchSSECKeyMessage) {
		t.Fatalf("body = %q, want AccessDenied mismatch message", body)
	}
}

func newSSECTestRequest(t *testing.T, clientKey []byte) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	keyB64 := base64.StdEncoding.EncodeToString(clientKey)
	md5Hash := md5.Sum(clientKey)
	keyMD5B64 := base64.StdEncoding.EncodeToString(md5Hash[:])
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	req.Header.Set("x-amz-server-side-encryption-customer-key", keyB64)
	req.Header.Set("x-amz-server-side-encryption-customer-key-MD5", keyMD5B64)
	return req
}

func containsAll(s string, substrings ...string) bool {
	for _, substring := range substrings {
		if !strings.Contains(s, substring) {
			return false
		}
	}
	return true
}
