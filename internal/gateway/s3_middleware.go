package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nexus/internal/auth"
	"nexus/internal/common"
)

func (g *S3Gateway) setCORSHeaders(w http.ResponseWriter, r *http.Request, bucket string) {
	origin := r.Header.Get("Origin")
	if origin != "" {
		bucketInfo, err := g.metadata.GetBucket(r.Context(), bucket)
		if err == nil && bucketInfo != nil && bucketInfo.CORS != nil && len(bucketInfo.CORS.AllowedOrigins) > 0 {
			for _, allowed := range bucketInfo.CORS.AllowedOrigins {
				if allowed == "*" || allowed == origin {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					break
				}
			}
		}
	}
	if w.Header().Get("Access-Control-Allow-Origin") == "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS, PATCH")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Content-Length, x-amz-content-sha256, x-amz-date, x-amz-security-token, x-amz-user-agent, x-amz-meta-*, x-amz-acl, x-amz-copy-source, x-amz-tagging, x-amz-server-side-encryption, x-amz-server-side-encryption-customer-algorithm, x-amz-server-side-encryption-customer-key, x-amz-server-side-encryption-customer-key-MD5, x-amz-checksum-crc32c, x-amz-checksum-sha256, x-amz-checksum-md5, x-amz-checksum-mode, X-Amz-Algorithm, X-Amz-Credential, X-Amz-Date, X-Amz-Expires, X-Amz-SignedHeaders, X-Amz-Signature, amz-sdk-invocation-id, amz-sdk-request, amz-sdk-retry, X-Nexus-Resumable, X-Nexus-Finalize, Upload-Offset, Upload-Length, Upload-Checksum")
	w.Header().Set("Access-Control-Max-Age", "3600")
	w.Header().Set("Access-Control-Expose-Headers", "ETag, X-Amz-Version-Id, X-Amz-Request-Id, X-Amz-Expiration, X-Amz-Checksum-CRC32C, X-Amz-Checksum-SHA256, X-Amz-Checksum-MD5, x-amz-server-side-encryption-customer-algorithm, x-amz-server-side-encryption-customer-key-MD5, X-Nexus-Upload-Id, Upload-Offset, Upload-Length, Upload-Checksum")
}

func (g *S3Gateway) validateBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	if !bucketNameRegex.MatchString(name) {
		return false
	}
	if strings.HasPrefix(name, "xn--") {
		return false
	}
	if strings.HasSuffix(name, "-s3alias") {
		return false
	}
	return true
}

func (g *S3Gateway) validateObjectKey(key string) bool {
	if len(key) > 1024 {
		return false
	}
	if pathTraversalPattern.MatchString(key) {
		return false
	}
	if controlCharPattern.MatchString(key) {
		return false
	}
	return true
}

func (g *S3Gateway) validateContentType(contentType string) bool {
	if contentType == "" {
		return true
	}
	blocked := []string{
		"application/x-msdos-program",
		"application/x-msdownload",
	}
	lower := strings.ToLower(contentType)
	for _, b := range blocked {
		if strings.HasPrefix(lower, b) {
			return false
		}
	}
	return true
}

func (g *S3Gateway) validatePresignedURLMethod(r *http.Request, expectedMethod string) error {
	if r.Method != expectedMethod {
		return fmt.Errorf("presigned URL method mismatch: expected %s, got %s", expectedMethod, r.Method)
	}
	return nil
}

func (g *S3Gateway) isBucketPublicRead(r *http.Request, bucket string) bool {
	if g.config != nil && g.config.Auth.AnonymousRead {
		return true
	}
	bucketInfo, err := g.metadata.GetBucket(r.Context(), bucket)
	if err != nil {
		return false
	}
	return bucketInfo.ACL == "public-read" || bucketInfo.ACL == "public-read-write"
}

func extractIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}

func extractVirtualHostedBucket(host, domains string) string {
	if domains == "" {
		return ""
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.ToLower(h)

	for _, domain := range strings.Split(domains, ",") {
		domain = strings.TrimSpace(strings.ToLower(domain))
		if domain == "" {
			continue
		}
		if !strings.HasSuffix(h, "."+domain) {
			continue
		}
		bucket := strings.TrimSuffix(h, "."+domain)
		if bucket == "" {
			continue
		}
		if bucketNameRegex.MatchString(bucket) {
			return bucket
		}
	}
	return ""
}

func isClientDisconnected(err error) bool {
	if err == nil {
		return false
	}
	if err == context.Canceled {
		return true
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "client disconnected") {
		return true
	}
	if strings.Contains(errMsg, "use of closed connection") {
		return true
	}
	return false
}

func parseRangeHeader(rangeHeader string, totalSize int64) (start, end int64, err error) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, fmt.Errorf("invalid range header format")
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range specification")
	}

	if parts[0] == "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range end")
		}
		start = totalSize - end
		if start < 0 {
			start = 0
		}
		end = totalSize - 1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range start")
		}
		if parts[1] == "" {
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid range end")
			}
		}
	}

	if start > end || start >= totalSize {
		return 0, 0, fmt.Errorf("range not satisfiable")
	}

	if end >= totalSize {
		end = totalSize - 1
	}

	return start, end, nil
}

func (g *S3Gateway) getIdentity(r *http.Request) (*auth.Identity, error) {
	if g.authProvider == nil {
		return nil, fmt.Errorf("authentication provider not configured")
	}
	return g.authProvider.Authenticate(r)
}

func (g *S3Gateway) getUserID(r *http.Request) string {
	identity, err := g.getIdentity(r)
	if err != nil || identity == nil {
		return "anonymous"
	}
	return identity.ID
}

func (g *S3Gateway) requireIdentity(r *http.Request, action, bucket, key string) (*auth.Identity, error) {
	identity, err := g.getIdentity(r)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, fmt.Errorf("authentication required")
	}
	if g.permChecker == nil {
		return identity, nil
	}
	if err := g.permChecker.Check(r.Context(), identity, action, bucket, key, r); err != nil {
		return nil, err
	}
	return identity, nil
}

func (g *S3Gateway) auditLog(r *http.Request, action, bucket, key, userID, result string, details map[string]interface{}) {
	ctx := context.Background()
	clientIP := ""
	userAgent := ""
	if r != nil {
		ctx = r.Context()
		clientIP = r.RemoteAddr
		userAgent = r.UserAgent()
	}
	requestID := common.GetRequestID(ctx)

	entry := map[string]interface{}{
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
		"request_id": requestID,
		"action":     action,
		"bucket":     bucket,
		"key":        key,
		"user":       userID,
		"result":     result,
		"client_ip":  clientIP,
		"user_agent": userAgent,
	}

	for k, v := range details {
		entry[k] = v
	}

	if g.config != nil && g.config.Logging.Format == "json" {
		data, _ := json.Marshal(entry)
		fmt.Printf("%s\n", string(data))
	}
}
