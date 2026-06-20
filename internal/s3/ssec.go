// Package s3 contains S3-protocol helpers shared by the gateway and services.
package s3

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
)

const (
	headerAlgorithm = "x-amz-server-side-encryption-customer-algorithm"
	headerKey       = "x-amz-server-side-encryption-customer-key"
	headerKeyMD5    = "x-amz-server-side-encryption-customer-key-MD5"
)

// ParseSSECHeaders parses and validates SSE-C headers from r. It returns the
// decoded 32-byte AES-256 client key, or nil if no SSE-C headers are present.
// Per S3 spec, the key-MD5 header is required when SSE-C is used.
func ParseSSECHeaders(r *http.Request) ([]byte, error) {
	algorithm := r.Header.Get(headerAlgorithm)
	keyB64 := r.Header.Get(headerKey)
	keyMD5B64 := r.Header.Get(headerKeyMD5)

	if algorithm == "" && keyB64 == "" && keyMD5B64 == "" {
		return nil, nil
	}

	if algorithm != "AES256" {
		return nil, fmt.Errorf("invalid SSE-C algorithm: must be AES256")
	}

	if keyB64 == "" {
		return nil, fmt.Errorf("SSE-C customer key is required when algorithm is specified")
	}

	clientKey, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("invalid SSE-C customer key: not valid base64")
	}

	if len(clientKey) != 32 {
		zero(clientKey)
		return nil, fmt.Errorf("invalid SSE-C customer key: must be 256-bit (32 bytes)")
	}

	if keyMD5B64 == "" {
		zero(clientKey)
		return nil, fmt.Errorf("SSE-C customer key MD5 is required")
	}

	expectedMD5, err := base64.StdEncoding.DecodeString(keyMD5B64)
	if err != nil {
		zero(clientKey)
		return nil, fmt.Errorf("invalid SSE-C customer key MD5: not valid base64")
	}

	computedMD5 := md5.Sum(clientKey)
	if subtle.ConstantTimeCompare(expectedMD5, computedMD5[:]) != 1 {
		zero(clientKey)
		return nil, fmt.Errorf("SSE-C customer key MD5 mismatch")
	}

	return clientKey, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
