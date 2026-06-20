package s3

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseSSECHeaders_NoHeaders(t *testing.T) {
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	key, err := ParseSSECHeaders(req)
	assert.NoError(t, err)
	assert.Nil(t, key)
}

func TestParseSSECHeaders_ValidHeaders(t *testing.T) {
	clientKey := make([]byte, 32)
	_, _ = rand.Read(clientKey)

	keyB64 := base64.StdEncoding.EncodeToString(clientKey)
	md5Hash := md5.Sum(clientKey)
	keyMD5B64 := base64.StdEncoding.EncodeToString(md5Hash[:])

	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set(headerAlgorithm, "AES256")
	req.Header.Set(headerKey, keyB64)
	req.Header.Set(headerKeyMD5, keyMD5B64)

	key, err := ParseSSECHeaders(req)
	assert.NoError(t, err)
	assert.Equal(t, clientKey, key)
}

func TestParseSSECHeaders_InvalidAlgorithm(t *testing.T) {
	clientKey := make([]byte, 32)
	_, _ = rand.Read(clientKey)

	keyB64 := base64.StdEncoding.EncodeToString(clientKey)
	md5Hash := md5.Sum(clientKey)
	keyMD5B64 := base64.StdEncoding.EncodeToString(md5Hash[:])

	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set(headerAlgorithm, "AES128")
	req.Header.Set(headerKey, keyB64)
	req.Header.Set(headerKeyMD5, keyMD5B64)

	key, err := ParseSSECHeaders(req)
	assert.Error(t, err)
	assert.Nil(t, key)
	assert.Contains(t, err.Error(), "AES256")
}

func TestParseSSECHeaders_InvalidKeySize(t *testing.T) {
	shortKey := make([]byte, 16)
	_, _ = rand.Read(shortKey)

	keyB64 := base64.StdEncoding.EncodeToString(shortKey)
	md5Hash := md5.Sum(shortKey)
	keyMD5B64 := base64.StdEncoding.EncodeToString(md5Hash[:])

	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set(headerAlgorithm, "AES256")
	req.Header.Set(headerKey, keyB64)
	req.Header.Set(headerKeyMD5, keyMD5B64)

	key, err := ParseSSECHeaders(req)
	assert.Error(t, err)
	assert.Nil(t, key)
	assert.Contains(t, err.Error(), "256-bit")
}

func TestParseSSECHeaders_MD5Mismatch(t *testing.T) {
	clientKey := make([]byte, 32)
	_, _ = rand.Read(clientKey)

	keyB64 := base64.StdEncoding.EncodeToString(clientKey)

	wrongKey := make([]byte, 32)
	_, _ = rand.Read(wrongKey)
	wrongMD5 := md5.Sum(wrongKey)
	keyMD5B64 := base64.StdEncoding.EncodeToString(wrongMD5[:])

	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set(headerAlgorithm, "AES256")
	req.Header.Set(headerKey, keyB64)
	req.Header.Set(headerKeyMD5, keyMD5B64)

	key, err := ParseSSECHeaders(req)
	assert.Error(t, err)
	assert.Nil(t, key)
	assert.Contains(t, err.Error(), "MD5 mismatch")
}

func TestParseSSECHeaders_InvalidBase64Key(t *testing.T) {
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set(headerAlgorithm, "AES256")
	req.Header.Set(headerKey, "not-valid-base64!!!")
	req.Header.Set(headerKeyMD5, "AAAA")

	key, err := ParseSSECHeaders(req)
	assert.Error(t, err)
	assert.Nil(t, key)
	assert.Contains(t, err.Error(), "base64")
}

func TestParseSSECHeaders_InvalidBase64MD5(t *testing.T) {
	clientKey := make([]byte, 32)
	_, _ = rand.Read(clientKey)

	keyB64 := base64.StdEncoding.EncodeToString(clientKey)

	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set(headerAlgorithm, "AES256")
	req.Header.Set(headerKey, keyB64)
	req.Header.Set(headerKeyMD5, "not-valid-base64!!!")

	key, err := ParseSSECHeaders(req)
	assert.Error(t, err)
	assert.Nil(t, key)
	assert.Contains(t, err.Error(), "base64")
}

func TestParseSSECHeaders_MissingMD5(t *testing.T) {
	clientKey := make([]byte, 32)
	_, _ = rand.Read(clientKey)

	keyB64 := base64.StdEncoding.EncodeToString(clientKey)

	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set(headerAlgorithm, "AES256")
	req.Header.Set(headerKey, keyB64)

	key, err := ParseSSECHeaders(req)
	assert.Error(t, err)
	assert.Nil(t, key)
	assert.Contains(t, err.Error(), "MD5 is required")
}

func TestParseSSECHeaders_AlgorithmOnlyNoKey(t *testing.T) {
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set(headerAlgorithm, "AES256")

	key, err := ParseSSECHeaders(req)
	assert.Error(t, err)
	assert.Nil(t, key)
	assert.Contains(t, err.Error(), "key is required")
}
