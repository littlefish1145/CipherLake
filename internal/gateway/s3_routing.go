package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"nexus/internal/auth"
	"nexus/internal/common"
)

type ctxKeyOriginalPath struct{}

func (g *S3Gateway) handleRequest(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	requestID := uuid.New().String()
	ctx := r.Context()
	ctx = common.WithRequestID(ctx, requestID)
	r = r.WithContext(ctx)

	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", uuid.New().String()[:16])

	var vhostBucket string
	if g.config != nil {
		vhostBucket = extractVirtualHostedBucket(r.Host, g.config.Node.Domain)
	}

	if vhostBucket != "" {
		originalPath := r.URL.Path
		r.URL.Path = "/" + vhostBucket + originalPath
		ctx = context.WithValue(r.Context(), ctxKeyOriginalPath{}, originalPath)
		r = r.WithContext(ctx)
	}

	corsBucket := vhostBucket
	g.setCORSHeaders(w, r, corsBucket)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if g.rateLimiter != nil {
		userID := g.getUserID(r)
		ip := extractIP(r)
		bucket := vhostBucket
		if bucket == "" {
			if len(r.URL.Path) > 1 {
				parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
				if len(parts) > 0 {
					bucket = parts[0]
				}
			}
		}
		contentLength, _ := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64)
		result := g.rateLimiter.Allow(ctx, ip, userID, bucket, r.Method, contentLength)
		if !result.Allowed {
			retryAfter := fmt.Sprintf("%d", result.RetryAfter)
			if result.RetryAfter <= 0 {
				retryAfter = "1"
			}
			w.Header().Set("Retry-After", retryAfter)
			w.Header().Set("X-RateLimit-Limit-Type", result.LimitType)
			g.writeError(w, http.StatusTooManyRequests, "SlowDown",
				fmt.Sprintf("Rate limit exceeded (%s). Please retry after %ss.", result.LimitType, retryAfter))
			return
		}
	}

	path := r.URL.Path
	method := r.Method

	if len(path) > 1024 {
		g.writeError(w, http.StatusBadRequest, "InvalidURI", "Object key too long")
		return
	}

	if pathTraversalPattern.MatchString(path) || controlCharPattern.MatchString(path) || encodedTraversalPattern.MatchString(r.URL.RawPath) || encodedTraversalPattern.MatchString(r.URL.RawQuery) {
		g.writeError(w, http.StatusBadRequest, "InvalidURI", "Invalid characters in path")
		return
	}

	var handler func(w http.ResponseWriter, r *http.Request) error

	if path == "/" {
		if method == "GET" {
			if _, err := g.requireIdentity(r, auth.ActionRead, "", ""); err != nil {
				g.writeError(w, http.StatusUnauthorized, "AccessDenied", err.Error())
				return
			}
			handler = g.bucketSvc.handleListBuckets
		} else if method == "POST" {
			if _, err := g.requireIdentity(r, auth.ActionWrite, "", ""); err != nil {
				g.writeError(w, http.StatusUnauthorized, "AccessDenied", err.Error())
				return
			}
			handler = g.handlePOST
		}
	} else if len(path) > 1 {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		bucket := parts[0]

		if !g.validateBucketName(bucket) {
			g.writeError(w, http.StatusBadRequest, "InvalidBucketName", "Bucket name must be between 3-63 characters, lowercase letters, numbers, hyphens, and periods")
			return
		}

		if len(parts) == 1 || (len(parts) == 2 && parts[1] == "") {
			handler = g.handleBucketOperations(bucket, method)
		} else if len(parts) == 2 {
			key := parts[1]
			if !g.validateObjectKey(key) {
				g.writeError(w, http.StatusBadRequest, "InvalidKey", "Object key contains invalid characters")
				return
			}
			handler = g.handleObjectOperations(bucket, key, method)
		}
	}

	if handler == nil {
		g.writeError(w, http.StatusNotFound, "NoSuchResource", "The specified resource does not exist.")
		return
	}

	if err := handler(w, r); err != nil {
		g.writeError(w, http.StatusInternalServerError, "InternalError", "An internal error occurred")
	}

	latency := time.Since(startTime)
	_ = latency

	if g.accessLog != nil {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		bucket := ""
		key := ""
		if len(parts) >= 1 {
			bucket = parts[0]
		}
		if len(parts) >= 2 {
			key = parts[1]
		}
		g.accessLog.Log(AccessLogEntry{
			RemoteIP:   getClientIP(r),
			UserID:     g.getUserID(r),
			Operation:  method,
			Bucket:     bucket,
			Key:        key,
			StatusCode: 200,
			BytesSent:  0,
			RequestID:  requestID,
		})
	}
}

func (g *S3Gateway) handlePOST(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "POST" {
		return fmt.Errorf("method not allowed")
	}

	query := r.URL.Query()
	if query.Has("vector_search") {
		return g.searchSvc.handleVectorSearch(w, r)
	}

	if query.Has("fts_search") {
		return g.searchSvc.handleFTSSearch(w, r)
	}

	if query.Has("resumable") && g.resumableHandler != nil {
		path := r.URL.Path
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		bucket := ""
		key := ""
		if len(parts) >= 1 {
			bucket = parts[0]
		}
		if len(parts) >= 2 {
			key = parts[1]
		}
		if bucket == "" || key == "" {
			return fmt.Errorf("bucket and key are required for resumable upload")
		}
		return g.resumableHandler.HandleCreateSession(w, r, bucket, key)
	}

	if query.Has("uploadId") && query.Has("finalize") && g.resumableHandler != nil {
		path := r.URL.Path
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		bucket := ""
		key := ""
		if len(parts) >= 1 {
			bucket = parts[0]
		}
		if len(parts) >= 2 {
			key = parts[1]
		}
		return g.resumableHandler.HandleFinalize(w, r, bucket, key)
	}

	if query.Has("uploads") {
		bucket := query.Get("bucket")
		key := query.Get("key")
		if bucket == "" {
			path := r.URL.Path
			if len(path) > 1 {
				parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
				if len(parts) >= 1 {
					bucket = parts[0]
				}
				if len(parts) >= 2 {
					key = parts[1]
				}
			}
		}
		return g.multipartSvc.HandleCreateMultipartUpload(w, r, bucket, key)
	}

	return fmt.Errorf("unsupported POST operation")
}

func (g *S3Gateway) handleBucketOperations(bucket, method string) func(w http.ResponseWriter, r *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		query := r.URL.Query()

		switch method {
		case "PUT":
			if query.Has("acl") {
				if _, err := g.requireIdentity(r, auth.ActionAdmin, bucket, ""); err != nil {
					return fmt.Errorf("access denied: %w", err)
				}
				return g.bucketSvc.handlePutBucketAcl(w, r, bucket)
			}
			if _, err := g.requireIdentity(r, auth.ActionWrite, bucket, ""); err != nil {
				return fmt.Errorf("access denied: %w", err)
			}
			return g.bucketSvc.handleCreateBucket(w, r, bucket)
		case "DELETE":
			if _, err := g.requireIdentity(r, auth.ActionAdmin, bucket, ""); err != nil {
				return fmt.Errorf("access denied: %w", err)
			}
			return g.bucketSvc.handleDeleteBucket(w, r, bucket)
		case "HEAD":
			if _, err := g.requireIdentity(r, auth.ActionRead, bucket, ""); err != nil {
				return fmt.Errorf("access denied: %w", err)
			}
			return g.bucketSvc.handleHeadBucket(w, r, bucket)
		case "POST":
			if query.Has("delete") {
				if _, err := g.requireIdentity(r, auth.ActionDelete, bucket, ""); err != nil {
					return fmt.Errorf("access denied: %w", err)
				}
				return g.objectSvc.handleDeleteObjects(w, r, bucket)
			}
			return fmt.Errorf("unsupported POST operation on bucket")
		case "GET":
			if query.Has("location") {
				return g.bucketSvc.handleGetBucketLocation(w, r, bucket)
			}
			if query.Has("fts_search") {
				return g.searchSvc.handleFTSSearch(w, r)
			}
			if query.Has("acl") {
				if _, err := g.requireIdentity(r, auth.ActionRead, bucket, ""); err != nil {
					return fmt.Errorf("access denied: %w", err)
				}
				return g.bucketSvc.handleGetBucketAcl(w, r, bucket)
			}
			if query.Has("versioning") {
				return g.bucketSvc.handleGetBucketVersioning(w, r, bucket)
			}
			if query.Has("uploads") {
				return g.multipartSvc.HandleListUploads(w, r, bucket)
			}
			if !g.isBucketPublicRead(r, bucket) {
				if _, err := g.requireIdentity(r, auth.ActionRead, bucket, ""); err != nil {
					return fmt.Errorf("access denied: %w", err)
				}
			}
			return g.bucketSvc.handleListObjects(w, r, bucket)
		default:
			return fmt.Errorf("method not allowed")
		}
	}
}

func (g *S3Gateway) handleObjectOperations(bucket, key, method string) func(w http.ResponseWriter, r *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		query := r.URL.Query()

		if query.Has("uploadId") && g.resumableHandler != nil && r.Header.Get("X-Nexus-Resumable") == "1" {
			switch method {
			case "PATCH":
				return g.resumableHandler.HandlePatch(w, r, bucket, key)
			case "HEAD":
				return g.resumableHandler.HandleHead(w, r, bucket, key)
			case "POST":
				if query.Has("finalize") {
					return g.resumableHandler.HandleFinalize(w, r, bucket, key)
				}
				return fmt.Errorf("unsupported POST operation for resumable upload")
			default:
				return fmt.Errorf("method not allowed for resumable upload")
			}
		}

		if query.Has("uploadId") {
			switch method {
			case "PUT":
				return g.multipartSvc.HandleUploadPart(w, r, bucket, key)
			case "POST":
				return g.multipartSvc.HandleCompleteMultipartUpload(w, r, bucket, key)
			case "DELETE":
				return g.multipartSvc.HandleAbortMultipartUpload(w, r, bucket, key)
			case "GET":
				return g.multipartSvc.HandleListParts(w, r, bucket, key)
			default:
				return fmt.Errorf("method not allowed for multipart upload")
			}
		}

		switch method {
		case "PUT":
			if r.Header.Get("x-amz-copy-source") != "" {
				return g.objectSvc.handleCopyObject(w, r, bucket, key)
			}
			return g.objectSvc.handlePutObject(w, r, bucket, key)
		case "GET":
			return g.objectSvc.handleGetObject(w, r, bucket, key)
		case "HEAD":
			return g.objectSvc.handleHeadObject(w, r, bucket, key)
		case "DELETE":
			return g.objectSvc.handleDeleteObject(w, r, bucket, key)
		case "POST":
			return g.objectSvc.handleObjectPOST(w, r, bucket, key)
		case "PATCH":
			if g.resumableHandler != nil && query.Has("uploadId") {
				return g.resumableHandler.HandlePatch(w, r, bucket, key)
			}
			return fmt.Errorf("method not allowed")
		default:
			return fmt.Errorf("method not allowed")
		}
	}
}
