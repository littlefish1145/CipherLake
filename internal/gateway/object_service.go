package gateway

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/internal/auth"
	"nexus/internal/common"
	"nexus/internal/events"
	"nexus/internal/metadata"
	"nexus/internal/pipeline"
	"nexus/internal/s3"
	"nexus/internal/services"
	"nexus/internal/taskqueue"

	"github.com/google/uuid"
)

type ObjectService struct {
	gateway *S3Gateway
}

func NewObjectService(gateway *S3Gateway) *ObjectService {
	return &ObjectService{gateway: gateway}
}

func (s *ObjectService) handlePutObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "PUT" {
		return fmt.Errorf("method not allowed")
	}

	isPresignedURL := r.URL.Query().Get("X-Amz-Credential") != ""
	if isPresignedURL {
		if err := s.gateway.validatePresignedURLMethod(r, "PUT"); err != nil {
			return err
		}
	}

	if _, err := s.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if !s.gateway.validateContentType(contentType) {
		return fmt.Errorf("unsupported content type: %s", contentType)
	}

	contentLength := r.ContentLength
	if contentLength < 0 {
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, s.gateway.config.Performance.MaxUploadBytes))
		if err != nil {
			return fmt.Errorf("failed to read request body: %w", err)
		}
		contentLength = int64(len(bodyBytes))
		r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
	}

	if s.gateway.config != nil && s.gateway.config.Performance.MaxUploadBytes > 0 {
		if contentLength > s.gateway.config.Performance.MaxUploadBytes {
			s.gateway.writeError(w, http.StatusRequestEntityTooLarge, "EntityTooLarge",
				fmt.Sprintf("Object size %d exceeds maximum allowed size %d",
					contentLength, s.gateway.config.Performance.MaxUploadBytes))
			return nil
		}
	}

	metadataMap := make(map[string]string)
	for k, values := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			metadataMap[strings.TrimPrefix(k, "x-amz-meta-")] = values[0]
		}
	}

	userID := s.gateway.getUserID(r)
	if userID == "" {
		userID = "anonymous"
	}

	etag := uuid.New().String()

	var providedChecksum string
	var providedChecksumType string

	if c := r.Header.Get("x-amz-checksum-crc32c"); c != "" {
		providedChecksum = c
		providedChecksumType = "crc32c"
	} else if c := r.Header.Get("x-amz-checksum-sha256"); c != "" {
		providedChecksum = c
		providedChecksumType = "sha256"
	} else if c := r.Header.Get("x-amz-checksum-md5"); c != "" {
		providedChecksum = c
		providedChecksumType = "md5"
	} else if c := r.Header.Get("Content-MD5"); c != "" {
		providedChecksum = c
		providedChecksumType = "md5"
	}

	sha256Hasher := sha256.New()
	var crc32cHasher hash.Hash
	var md5Hasher hash.Hash

	if providedChecksumType == "crc32c" {
		crc32cHasher = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	}
	if providedChecksumType == "md5" {
		md5Hasher = md5.New()
	}

	var hashWriters []io.Writer
	hashWriters = append(hashWriters, sha256Hasher)
	if crc32cHasher != nil {
		hashWriters = append(hashWriters, crc32cHasher)
	}
	if md5Hasher != nil {
		hashWriters = append(hashWriters, md5Hasher)
	}
	multiWriter := io.MultiWriter(hashWriters...)

	var plaintextReader io.Reader = io.TeeReader(r.Body, multiWriter)

	var dataReader io.Reader = plaintextReader
	var encryptedDEKMetadata []byte
	var encrypted bool
	var actualStorageSize int64 = contentLength
	var ssecUsed bool
	var ssecKeySHA256 string

	clientKey, ssecErr := s3.ParseSSECHeaders(r)
	if ssecErr != nil {
		s.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", ssecErr.Error())
		return nil
	}

	if clientKey != nil && s.gateway.cryptoCoordinator != nil {
		ssecUsed = true
		ssecKeySHA256 = services.ComputeSSECKeySHA256(clientKey)

		encryptedReader, ssecMetadata, ciphertextSize, err := s.gateway.cryptoCoordinator.EncryptWithClientKey(r.Context(), plaintextReader, clientKey, contentLength)
		for i := range clientKey {
			clientKey[i] = 0
		}
		if err != nil {
			return fmt.Errorf("SSE-C encryption failed")
		}
		dataReader = encryptedReader
		encryptedDEKMetadata = ssecMetadata
		encrypted = true
		actualStorageSize = ciphertextSize
	} else if clientKey != nil {
		for i := range clientKey {
			clientKey[i] = 0
		}
		return fmt.Errorf("SSE-C encryption requested but encryption services are not enabled")
	} else if s.gateway.cryptoCoordinator != nil && s.gateway.config != nil && s.gateway.config.CryptoServices.Enabled {
		encryptedReader, _, metadata, ciphertextSize, err := s.gateway.cryptoCoordinator.EncryptOperation(r.Context(), userID, bucket, key, plaintextReader, contentLength)
		if err != nil {
			return fmt.Errorf("encryption failed: %w", err)
		}
		dataReader = encryptedReader
		encryptedDEKMetadata = metadata
		encrypted = true
		actualStorageSize = ciphertextSize
	}

	objMetadata := &common.ObjectMetadata{
		Key:            key,
		Bucket:         bucket,
		Size:           contentLength,
		ContentType:    contentType,
		ETag:           etag,
		UserMetadata:   metadataMap,
		StorageTier:    common.TierHot,
		CreatedAt:      time.Now(),
		ModifiedAt:     time.Now(),
		AccessCount:    0,
		LastAccessedAt: time.Now(),
		Encrypted:      encrypted,
		VersionID:      uuid.New().String(),
	}

	storageTier := common.TierHot
	if err := s.gateway.store.Put(r.Context(), bucket, key, dataReader, actualStorageSize, storageTier, objMetadata); err != nil {
		return fmt.Errorf("failed to store object: %w", err)
	}

	computedSHA256 := base64.StdEncoding.EncodeToString(sha256Hasher.Sum(nil))

	var storedChecksum string
	var storedChecksumType string

	switch providedChecksumType {
	case "crc32c":
		storedChecksum = base64.StdEncoding.EncodeToString(crc32cHasher.Sum(nil))
		storedChecksumType = "crc32c"
		if providedChecksum != storedChecksum {
			s.gateway.store.Delete(r.Context(), bucket, key, storageTier)
			s.gateway.writeError(w, http.StatusBadRequest, "BadDigest",
				fmt.Sprintf("CRC32C checksum mismatch: expected %s, got %s", providedChecksum, storedChecksum))
			return nil
		}
	case "sha256":
		storedChecksum = computedSHA256
		storedChecksumType = "sha256"
		if providedChecksum != storedChecksum {
			s.gateway.store.Delete(r.Context(), bucket, key, storageTier)
			s.gateway.writeError(w, http.StatusBadRequest, "BadDigest",
				fmt.Sprintf("SHA256 checksum mismatch: expected %s, got %s", providedChecksum, storedChecksum))
			return nil
		}
	case "md5":
		storedChecksum = base64.StdEncoding.EncodeToString(md5Hasher.Sum(nil))
		storedChecksumType = "md5"
		if providedChecksum != storedChecksum {
			s.gateway.store.Delete(r.Context(), bucket, key, storageTier)
			s.gateway.writeError(w, http.StatusBadRequest, "BadDigest",
				fmt.Sprintf("MD5 checksum mismatch: expected %s, got %s", providedChecksum, storedChecksum))
			return nil
		}
	default:
		storedChecksum = computedSHA256
		storedChecksumType = "sha256"
	}

	meta := &metadata.ObjectMetadata{
		Key:            key,
		Bucket:         bucket,
		Size:           contentLength,
		ContentType:    contentType,
		ETag:           etag,
		UserMetadata:   metadataMap,
		StorageTier:    int(common.TierHot),
		CreatedAt:      time.Now(),
		ModifiedAt:     time.Now(),
		AccessCount:    0,
		LastAccessedAt: time.Now(),
		Encrypted:      encrypted,
		VersionID:      objMetadata.VersionID,
		IsLatest:       true,
		ObjectStatus:   "active",
		Checksum:       storedChecksum,
		ChecksumType:   storedChecksumType,
		SSECUsed:       ssecUsed,
		SSECKeySHA256:  ssecKeySHA256,
		SSECAlgorithm: func() string {
			if ssecUsed {
				return "AES256"
			}
			return ""
		}(),
	}

	if encryptedDEKMetadata != nil {
		meta.EncryptedDEK = encryptedDEKMetadata
	}

	if ssecUsed && s.gateway.config != nil && s.gateway.config.Encryption.EnableDedup {
	}

	if err := s.gateway.metadata.PutObject(r.Context(), bucket, key, meta); err != nil {
		s.gateway.store.Delete(r.Context(), bucket, key, storageTier)
		return fmt.Errorf("failed to store metadata: %w", err)
	}

	if s.gateway.tiering != nil {
		s.gateway.tiering.RecordAccess(r.Context(), bucket, key, "PUT", userID)
	}

	if s.gateway.vector != nil && s.gateway.config.Vector.Enabled {
		if r.Header.Get("X-Vectorize") != "false" {
			s.gateway.submitBackgroundTask(r.Context(), taskqueue.KindVectorize, taskqueue.VectorizePayload{
				Bucket:      bucket,
				Key:         key,
				ContentType: contentType,
				Metadata:    metadataMap,
				UserID:      userID,
			})
		}
	}

	if s.gateway.ftsIndex != nil && s.gateway.config.FTS.Enabled {
		if r.Header.Get("X-FTS-Index") != "false" {
			s.gateway.submitBackgroundTask(r.Context(), taskqueue.KindFTS, taskqueue.FTSPayload{
				Bucket:      bucket,
				Key:         key,
				ContentType: contentType,
				VersionID:   objMetadata.VersionID,
			})
		}
	}

	if s.gateway.pipeline != nil {
		s.gateway.submitBackgroundTask(r.Context(), taskqueue.KindPipeline, taskqueue.PipelinePayload{
			Bucket:      bucket,
			Key:         key,
			ContentType: contentType,
			Metadata:    metadataMap,
		})
	}

	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("x-amz-version-id", objMetadata.VersionID)
	if ssecUsed {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
		if keyMD5 := r.Header.Get("x-amz-server-side-encryption-customer-key-MD5"); keyMD5 != "" {
			w.Header().Set("x-amz-server-side-encryption-customer-key-MD5", keyMD5)
		}
	} else if encrypted {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}
	switch storedChecksumType {
	case "crc32c":
		w.Header().Set("x-amz-checksum-crc32c", storedChecksum)
	case "sha256":
		w.Header().Set("x-amz-checksum-sha256", storedChecksum)
	case "md5":
		w.Header().Set("x-amz-checksum-md5", storedChecksum)
	}
	w.WriteHeader(http.StatusOK)

	if s.gateway.metrics != nil {
		s.gateway.metrics.RecordPutObject(bucket, "success")
	}

	auditDetails := map[string]interface{}{
		"size":         contentLength,
		"content_type": contentType,
		"encrypted":    encrypted,
	}
	if ssecUsed {
		auditDetails["sse_c_used"] = true
	}
	s.gateway.auditLog(r, "PUT", bucket, key, userID, "success", auditDetails)

	if s.gateway.eventBus != nil {
		s.gateway.eventBus.Publish(r.Context(), &events.Event{
			EventType:    "s3:ObjectCreated:Put",
			Bucket:       bucket,
			Key:          key,
			VersionID:    objMetadata.VersionID,
			ETag:         etag,
			Size:         contentLength,
			RequesterARN: userID,
			SourceIP:     extractIP(r),
		})
	}

	return nil
}

func (s *ObjectService) handleGetObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "GET" {
		return fmt.Errorf("method not allowed")
	}

	isPresignedURL := r.URL.Query().Get("X-Amz-Credential") != ""

	if isPresignedURL {
		if err := s.gateway.validatePresignedURLMethod(r, "GET"); err != nil {
			return err
		}
	}

	if !isPresignedURL && !s.gateway.isBucketPublicRead(r, bucket) {
		if _, err := s.gateway.requireIdentity(r, auth.ActionRead, bucket, key); err != nil {
			return fmt.Errorf("access denied: %w", err)
		}
	}

	userID := s.gateway.getUserID(r)
	if userID == "" {
		userID = "anonymous"
	}

	if s.gateway.tiering != nil {
		s.gateway.tiering.RecordAccess(r.Context(), bucket, key, "GET", userID)
	}

	objMetadata, err := s.gateway.metadata.GetObject(r.Context(), bucket, key)
	if err != nil {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchKey", fmt.Sprintf("Object '%s' not found", key))
		return nil
	}

	if objMetadata.DeleteMarker {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchKey", "Object has been deleted")
		return nil
	}

	etag := `"` + objMetadata.ETag + `"`
	ifNoneMatch := r.Header.Get("If-None-Match")
	if ifNoneMatch != "" {
		if ifNoneMatch == "*" || ifNoneMatch == etag {
			w.WriteHeader(http.StatusNotModified)
			return nil
		}
	}

	ifMatch := r.Header.Get("If-Match")
	if ifMatch != "" && ifMatch != "*" && ifMatch != etag {
		s.gateway.writeError(w, http.StatusPreconditionFailed, "PreconditionFailed", "ETag does not match")
		return nil
	}

	ifModifiedSince := r.Header.Get("If-Modified-Since")
	if ifModifiedSince != "" {
		ifModTime, err := time.Parse(http.TimeFormat, ifModifiedSince)
		if err == nil && !objMetadata.ModifiedAt.After(ifModTime) {
			w.WriteHeader(http.StatusNotModified)
			return nil
		}
	}

	rangeHeader := r.Header.Get("Range")
	cacheKey := bucket + "/" + key

	if s.gateway.objectCache != nil && objMetadata.Size < 1<<20 && rangeHeader == "" && !objMetadata.Encrypted {
		if cachedData, found := s.gateway.objectCache.Get(r.Context(), cacheKey); found {
			w.Header().Set("Content-Type", objMetadata.ContentType)
			w.Header().Set("Content-Length", strconv.FormatInt(int64(len(cachedData)), 10))
			w.Header().Set("ETag", etag)
			w.Header().Set("Last-Modified", objMetadata.ModifiedAt.Format(http.TimeFormat))
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Cache-Control", "max-age=3600")
			w.Header().Set("X-Cache", "HIT")
			for k, v := range objMetadata.UserMetadata {
				w.Header().Set("X-Amz-Meta-"+k, v)
			}
			setChecksumResponseHeaders(w, objMetadata, r)
			w.WriteHeader(http.StatusOK)
			w.Write(cachedData)
			return nil
		}
	}

	storageTier := common.StorageTier(objMetadata.StorageTier)
	reader, _, err := s.gateway.store.Get(r.Context(), bucket, key, storageTier)
	if err != nil {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchKey", fmt.Sprintf("Object '%s' not found", key))
		return nil
	}
	defer reader.Close()

	var dataReader io.Reader = reader
	var contentLength = objMetadata.Size

	if objMetadata.SSECUsed {
		clientKey, ssecErr := s3.ParseSSECHeaders(r)
		if ssecErr != nil {
			s.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", "Invalid SSE-C customer key headers")
			return nil
		}
		if clientKey == nil {
			s.gateway.writeError(w, http.StatusForbidden, "AccessDenied", "Object encrypted with SSE-C requires customer key headers")
			return nil
		}
		keySHA256 := services.ComputeSSECKeySHA256(clientKey)
		if subtle.ConstantTimeCompare([]byte(keySHA256), []byte(objMetadata.SSECKeySHA256)) != 1 {
			s.gateway.writeError(w, http.StatusForbidden, "AccessDenied", "The provided SSE-C key does not match the key used to encrypt the object")
			for i := range clientKey {
				clientKey[i] = 0
			}
			return nil
		}
		if s.gateway.cryptoCoordinator != nil && len(objMetadata.EncryptedDEK) > 0 {
			decryptedReader, err := s.gateway.cryptoCoordinator.DecryptWithClientKey(r.Context(), reader, clientKey, objMetadata.EncryptedDEK, objMetadata.Size)
			for i := range clientKey {
				clientKey[i] = 0
			}
			if err != nil {
				s.gateway.writeError(w, http.StatusInternalServerError, "InternalError", "An internal error occurred while processing the object")
				return nil
			}
			dataReader = decryptedReader
		} else {
			for i := range clientKey {
				clientKey[i] = 0
			}
		}
	} else if objMetadata.Encrypted && s.gateway.cryptoCoordinator != nil && len(objMetadata.EncryptedDEK) > 0 {
		encryptedDEKMetadata := objMetadata.EncryptedDEK

		rawFallback := r.Header.Get("x-amz-raw-decryption-fallback") == "true"
		var rawBackup []byte
		if rawFallback {
			rawBackup, err = io.ReadAll(io.LimitReader(reader, s.gateway.config.Performance.MaxUploadBytes))
			if err != nil {
				s.gateway.writeError(w, http.StatusInternalServerError, "InternalError", "An internal error occurred while processing the object")
				return nil
			}
			reader = io.NopCloser(bytes.NewReader(rawBackup))
		}

		decryptedReader, err := s.gateway.cryptoCoordinator.DecryptOperation(r.Context(), userID, bucket, key, reader, "", encryptedDEKMetadata)
		if err != nil {
			if rawFallback && rawBackup != nil {
				w.Header().Set("x-amz-decryption-fallback", "true")
				dataReader = bytes.NewReader(rawBackup)
				contentLength = int64(len(rawBackup))
			} else {
				s.gateway.writeError(w, http.StatusInternalServerError, "InternalError", "An internal error occurred while processing the object")
				return nil
			}
		} else {
			dataReader = decryptedReader
		}
	}

	// Execute on_get pipelines (watermark, transformation, etc.)
	if s.gateway.pipeline != nil {
		onGetPipelines := s.gateway.pipeline.GetMatchingPipelines(r.Context(), pipeline.TriggerOnGet, objMetadata.ContentType, objMetadata.UserMetadata)
		for _, p := range onGetPipelines {
			input := &pipeline.ObjectInput{
				Key:          key,
				Bucket:       bucket,
				Content:      dataReader,
				Size:         contentLength,
				ContentType:  objMetadata.ContentType,
				UserMetadata: objMetadata.UserMetadata,
			}
			result, err := s.gateway.pipeline.Execute(r.Context(), p.Name, input)
			if err != nil {
				zap.L().Error("on_get pipeline failed", zap.String("pipeline", p.Name), zap.Error(err))
				continue
			}
			if result != nil && len(result.Outputs) > 0 {
				dataReader = result.Outputs[0].Content
				if result.Outputs[0].Size > 0 {
					contentLength = result.Outputs[0].Size
				}
				if result.Outputs[0].ContentType != "" {
					objMetadata.ContentType = result.Outputs[0].ContentType
				}
			}
		}
	}

	var statusCode = http.StatusOK
	var contentRange string

	if rangeHeader != "" {
		start, end, err := parseRangeHeader(rangeHeader, objMetadata.Size)
		if err != nil {
			s.gateway.writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", err.Error())
			return nil
		}

		if seeker, ok := dataReader.(io.Seeker); ok {
			seeker.Seek(start, io.SeekStart)
			dataReader = io.LimitReader(dataReader, end-start+1)
		} else {
			discard := start
			buf := make([]byte, 32*1024)
			for discard > 0 {
				n := int64(len(buf))
				if n > discard {
					n = discard
				}
				read, err := io.ReadFull(dataReader, buf[:n])
				if err != nil {
					break
				}
				discard -= int64(read)
			}
			dataReader = io.LimitReader(dataReader, end-start+1)
		}

		contentLength = end - start + 1
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, objMetadata.Size)
		statusCode = http.StatusPartialContent
	}

	w.Header().Set("Content-Type", objMetadata.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", objMetadata.ModifiedAt.Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "max-age=3600")

	if s.gateway.objectCache != nil {
		w.Header().Set("X-Cache", "MISS")
	}

	if objMetadata.SSECUsed {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
		if keyMD5 := r.Header.Get("x-amz-server-side-encryption-customer-key-MD5"); keyMD5 != "" {
			w.Header().Set("x-amz-server-side-encryption-customer-key-MD5", keyMD5)
		}
	} else if objMetadata.Encrypted {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}

	if contentRange != "" {
		w.Header().Set("Content-Range", contentRange)
	}

	for k, v := range objMetadata.UserMetadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}

	setChecksumResponseHeaders(w, objMetadata, r)

	w.WriteHeader(statusCode)

	if s.gateway.objectCache != nil && objMetadata.Size < 1<<20 && rangeHeader == "" && !objMetadata.Encrypted {
		data, err := io.ReadAll(io.LimitReader(dataReader, 1<<20))
		if err == nil {
			s.gateway.objectCache.Set(r.Context(), cacheKey, data)
			w.Write(data)
			return nil
		}
	}

	io.Copy(w, dataReader)

	if s.gateway.metrics != nil {
		s.gateway.metrics.RecordGetObject(bucket, "success", contentLength)
	}

	return nil
}

func (s *ObjectService) handleHeadObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "HEAD" {
		return fmt.Errorf("method not allowed")
	}

	if !s.gateway.isBucketPublicRead(r, bucket) {
		if _, err := s.gateway.requireIdentity(r, auth.ActionRead, bucket, key); err != nil {
			return fmt.Errorf("access denied: %w", err)
		}
	}

	objMetadata, err := s.gateway.metadata.GetObject(r.Context(), bucket, key)
	if err != nil {
		return fmt.Errorf("object not found: %w", err)
	}

	if objMetadata.SSECUsed {
		clientKey, ssecErr := s3.ParseSSECHeaders(r)
		if ssecErr != nil {
			s.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", ssecErr.Error())
			return nil
		}
		if clientKey == nil {
			s.gateway.writeError(w, http.StatusForbidden, "AccessDenied", "Object encrypted with SSE-C requires customer key headers")
			return nil
		}
		keySHA256 := services.ComputeSSECKeySHA256(clientKey)
		if subtle.ConstantTimeCompare([]byte(keySHA256), []byte(objMetadata.SSECKeySHA256)) != 1 {
			s.gateway.writeError(w, http.StatusForbidden, "AccessDenied", "The provided SSE-C key does not match the key used to encrypt the object")
			return nil
		}
		for i := range clientKey {
			clientKey[i] = 0
		}
	}

	w.Header().Set("Content-Type", objMetadata.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(objMetadata.Size, 10))
	w.Header().Set("ETag", `"`+objMetadata.ETag+`"`)
	w.Header().Set("Last-Modified", objMetadata.ModifiedAt.Format(http.TimeFormat))
	w.Header().Set("X-Amz-Storage-Class", common.StorageTier(objMetadata.StorageTier).String())

	if objMetadata.SSECUsed {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
		if keyMD5 := r.Header.Get("x-amz-server-side-encryption-customer-key-MD5"); keyMD5 != "" {
			w.Header().Set("x-amz-server-side-encryption-customer-key-MD5", keyMD5)
		}
	} else if objMetadata.Encrypted {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}

	for k, v := range objMetadata.UserMetadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}

	setChecksumResponseHeaders(w, objMetadata, r)

	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *ObjectService) handleDeleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "DELETE" {
		return fmt.Errorf("method not allowed")
	}

	if _, err := s.gateway.requireIdentity(r, auth.ActionDelete, bucket, key); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	objMetadata, err := s.gateway.metadata.GetObject(r.Context(), bucket, key)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	if r.Header.Get("x-amz-bypass-governance-retention") != "true" {
		if objMetadata.RetainUntil != nil && objMetadata.RetainUntil.After(time.Now()) {
			s.gateway.writeError(w, http.StatusForbidden, "ObjectLocked", "Object is under retention lock")
			return nil
		}
	}

	softDelete := r.Header.Get("x-amz-delete-marker") != "false"
	versioningEnabled := false

	if bucketInfo, err := s.gateway.metadata.GetBucket(r.Context(), bucket); err == nil {
		versioningEnabled = bucketInfo.Versioning
	}

	if softDelete && versioningEnabled {
		deleteMarker := &metadata.ObjectMetadata{
			Key:          key,
			Bucket:       bucket,
			Size:         0,
			ContentType:  "application/octet-stream",
			ETag:         uuid.New().String(),
			StorageTier:  objMetadata.StorageTier,
			CreatedAt:    time.Now(),
			ModifiedAt:   time.Now(),
			DeleteMarker: true,
			VersionID:    uuid.New().String(),
			IsLatest:     true,
			ObjectStatus: "delete-marker",
		}

		if err := s.gateway.metadata.PutObject(r.Context(), bucket, key, deleteMarker); err != nil {
			return fmt.Errorf("failed to create delete marker: %w", err)
		}

		w.Header().Set("x-amz-delete-marker", "true")
		w.Header().Set("x-amz-version-id", deleteMarker.VersionID)
	} else {
		storageTier := common.StorageTier(objMetadata.StorageTier)
		s.gateway.store.Delete(r.Context(), bucket, key, storageTier)

		if s.gateway.vector != nil {
			s.gateway.vector.DeleteVector(r.Context(), bucket, key)
		}

		if s.gateway.ftsIndex != nil {
			s.gateway.ftsIndex.DeleteDocumentByKey(bucket, key)
		}

		if s.gateway.objectCache != nil {
			s.gateway.objectCache.Delete(r.Context(), bucket+"/"+key)
		}

		s.gateway.metadata.DeleteObject(r.Context(), bucket, key)
	}

	userID := s.gateway.getUserID(r)
	s.gateway.auditLog(r, "DELETE", bucket, key, userID, "success", map[string]interface{}{
		"soft_delete": softDelete && versioningEnabled,
	})

	if s.gateway.eventBus != nil {
		s.gateway.eventBus.Publish(r.Context(), &events.Event{
			EventType:    "s3:ObjectRemoved:Delete",
			Bucket:       bucket,
			Key:          key,
			VersionID:    objMetadata.VersionID,
			ETag:         objMetadata.ETag,
			Size:         objMetadata.Size,
			RequesterARN: userID,
			SourceIP:     extractIP(r),
		})
	}

	w.WriteHeader(http.StatusNoContent)

	if s.gateway.metrics != nil {
		s.gateway.metrics.RecordPutObject(bucket, "delete")
	}

	return nil
}

func (s *ObjectService) handleDeleteObjects(w http.ResponseWriter, r *http.Request, bucket string) error {
	if r.Method != "POST" {
		return fmt.Errorf("method not allowed")
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}

	var req DeleteObjectsRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("failed to parse delete request: %w", err)
	}

	userID := s.gateway.getUserID(r)
	if userID == "" {
		userID = "anonymous"
	}

	result := DeleteObjectsResult{}

	for _, obj := range req.Objects {
		objMetadata, err := s.gateway.metadata.GetObject(r.Context(), bucket, obj.Key)
		if err != nil {
			if !req.Quiet {
				result.Error = append(result.Error, DeleteObjectsError{
					Key:     obj.Key,
					Code:    "NoSuchKey",
					Message: fmt.Sprintf("Object '%s' not found", obj.Key),
				})
			}
			continue
		}

		storageTier := common.StorageTier(objMetadata.StorageTier)
		s.gateway.store.Delete(r.Context(), bucket, obj.Key, storageTier)

		if s.gateway.vector != nil {
			s.gateway.vector.DeleteVector(r.Context(), bucket, obj.Key)
		}

		if s.gateway.ftsIndex != nil {
			s.gateway.ftsIndex.DeleteDocumentByKey(bucket, obj.Key)
		}

		if s.gateway.objectCache != nil {
			s.gateway.objectCache.Delete(r.Context(), bucket+"/"+obj.Key)
		}

		s.gateway.metadata.DeleteObject(r.Context(), bucket, obj.Key)

		if s.gateway.eventBus != nil {
			s.gateway.eventBus.Publish(r.Context(), &events.Event{
				EventType:    "s3:ObjectRemoved:Delete",
				Bucket:       bucket,
				Key:          obj.Key,
				VersionID:    objMetadata.VersionID,
				ETag:         objMetadata.ETag,
				Size:         objMetadata.Size,
				RequesterARN: userID,
				SourceIP:     r.RemoteAddr,
			})
		}

		result.Deleted = append(result.Deleted, DeleteObjectsDeleted{
			Key:       obj.Key,
			VersionID: objMetadata.VersionID,
		})
	}

	s.gateway.auditLog(r, "DELETE_OBJECTS", bucket, "", userID, "success", map[string]interface{}{
		"count": len(req.Objects),
	})

	return s.gateway.writeXML(w, http.StatusOK, result)
}

func (s *ObjectService) handleCopyObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if r.Method != "PUT" {
		return fmt.Errorf("method not allowed")
	}

	if _, err := s.gateway.requireIdentity(r, auth.ActionWrite, bucket, key); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	copySource := r.Header.Get("x-amz-copy-source")
	if copySource == "" {
		return fmt.Errorf("missing x-amz-copy-source header")
	}

	copySource = strings.TrimPrefix(copySource, "/")
	parts := strings.SplitN(copySource, "/", 2)
	if len(parts) < 2 {
		return fmt.Errorf("invalid copy source format, expected /bucket/key")
	}
	srcBucket := parts[0]
	srcKey := parts[1]

	if _, err := s.gateway.requireIdentity(r, auth.ActionRead, srcBucket, srcKey); err != nil {
		return fmt.Errorf("access denied on source: %w", err)
	}

	srcMeta, err := s.gateway.metadata.GetObject(r.Context(), srcBucket, srcKey)
	if err != nil {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchKey", fmt.Sprintf("Source object '%s/%s' not found", srcBucket, srcKey))
		return nil
	}

	if srcMeta.DeleteMarker {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchKey", "Source object has been deleted")
		return nil
	}

	userID := s.gateway.getUserID(r)
	if userID == "" {
		userID = "anonymous"
	}

	srcStorageTier := common.StorageTier(srcMeta.StorageTier)
	srcReader, _, err := s.gateway.store.Get(r.Context(), srcBucket, srcKey, srcStorageTier)
	if err != nil {
		return fmt.Errorf("failed to read source object: %w", err)
	}
	defer srcReader.Close()

	metadataMap := make(map[string]string)
	for k, v := range srcMeta.UserMetadata {
		metadataMap[k] = v
	}
	for k, values := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			metadataMap[strings.TrimPrefix(k, "x-amz-meta-")] = values[0]
		}
	}

	contentType := srcMeta.ContentType
	if ct := r.Header.Get("Content-Type"); ct != "" {
		contentType = ct
	}

	etag := uuid.New().String()
	versionID := uuid.New().String()

	objMetadata := &common.ObjectMetadata{
		Key:            key,
		Bucket:         bucket,
		Size:           srcMeta.Size,
		ContentType:    contentType,
		ETag:           etag,
		UserMetadata:   metadataMap,
		StorageTier:    common.TierHot,
		CreatedAt:      time.Now(),
		ModifiedAt:     time.Now(),
		AccessCount:    0,
		LastAccessedAt: time.Now(),
		Encrypted:      srcMeta.Encrypted,
		VersionID:      versionID,
	}

	storageTier := common.TierHot
	if err := s.gateway.store.Put(r.Context(), bucket, key, srcReader, srcMeta.Size, storageTier, objMetadata); err != nil {
		return fmt.Errorf("failed to copy object: %w", err)
	}

	meta := &metadata.ObjectMetadata{
		Key:            key,
		Bucket:         bucket,
		Size:           srcMeta.Size,
		ContentType:    contentType,
		ETag:           etag,
		UserMetadata:   metadataMap,
		StorageTier:    int(common.TierHot),
		CreatedAt:      time.Now(),
		ModifiedAt:     time.Now(),
		AccessCount:    0,
		LastAccessedAt: time.Now(),
		Encrypted:      srcMeta.Encrypted,
		VersionID:      versionID,
		IsLatest:       true,
		ObjectStatus:   "active",
	}

	if err := s.gateway.metadata.PutObject(r.Context(), bucket, key, meta); err != nil {
		return fmt.Errorf("failed to store metadata: %w", err)
	}

	if s.gateway.tiering != nil {
		s.gateway.tiering.RecordAccess(r.Context(), bucket, key, "PUT", userID)
	}

	if s.gateway.eventBus != nil {
		s.gateway.eventBus.Publish(r.Context(), &events.Event{
			EventType:    "s3:ObjectCreated:Copy",
			Bucket:       bucket,
			Key:          key,
			VersionID:    versionID,
			ETag:         etag,
			Size:         srcMeta.Size,
			RequesterARN: userID,
			SourceIP:     r.RemoteAddr,
		})
	}

	s.gateway.auditLog(r, "COPY", bucket, key, userID, "success", map[string]interface{}{
		"source_bucket": srcBucket,
		"source_key":    srcKey,
		"size":          srcMeta.Size,
	})

	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("x-amz-version-id", versionID)
	w.Header().Set("x-amz-copy-source-version-id", srcMeta.VersionID)

	output := CopyObjectResult{
		LastModified: time.Now().Format(time.RFC3339),
		ETag:         `"` + etag + `"`,
	}

	return s.gateway.writeXML(w, http.StatusOK, output)
}

func (s *ObjectService) handleObjectPOST(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	query := r.URL.Query()
	if query.Has("vector_search") {
		return s.gateway.searchSvc.handleVectorSearchForObject(w, r, bucket, key)
	}

	return fmt.Errorf("unsupported POST operation")
}
