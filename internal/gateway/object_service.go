package gateway

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cipherlake/internal/auth"
	"cipherlake/internal/common"
	"cipherlake/internal/events"
	"cipherlake/internal/metadata"
	"cipherlake/internal/s3"
	"cipherlake/internal/services"

	"github.com/google/uuid"
)

type ObjectService struct {
	gateway    *S3Gateway
	writeTasks objectWriteTaskScheduler
}

func NewObjectService(gateway *S3Gateway) *ObjectService {
	writeTasks := gateway.writeTasks
	if writeTasks == nil {
		writeTasks = newGatewayObjectWriteTaskScheduler(gateway)
	}
	return &ObjectService{
		gateway:    gateway,
		writeTasks: writeTasks,
	}
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

	contentType, err := s.gateway.parseObjectContentType(r.Header)
	if err != nil {
		return err
	}

	contentLength, err := s.gateway.resolveUploadContentLength(r)
	if err != nil {
		if tooLarge, ok := err.(*uploadTooLargeError); ok {
			s.gateway.writeError(w, http.StatusRequestEntityTooLarge, "EntityTooLarge",
				fmt.Sprintf("Object size %d exceeds maximum allowed size %d", tooLarge.size, tooLarge.max))
			return nil
		}
		return err
	}

	metadataMap := parseUserMetadata(r.Header)

	userID := s.gateway.requestUserID(r)

	etag := uuid.New().String()

	checksumRecorder := newObjectChecksumRecorder(r.Header)
	var plaintextReader io.Reader = io.TeeReader(r.Body, checksumRecorder.Writer())

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
		zeroClientKey(clientKey)
		if err != nil {
			return fmt.Errorf("SSE-C encryption failed")
		}
		dataReader = encryptedReader
		encryptedDEKMetadata = ssecMetadata
		encrypted = true
		actualStorageSize = ciphertextSize
	} else if clientKey != nil {
		zeroClientKey(clientKey)
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

	objMetadata, meta := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:               bucket,
		Key:                  key,
		Size:                 contentLength,
		ContentType:          contentType,
		ETag:                 etag,
		UserMetadata:         metadataMap,
		StorageTier:          common.TierHot,
		Encrypted:            encrypted,
		EncryptedDEK:         encryptedDEKMetadata,
		SSECUsed:             ssecUsed,
		SSECKeySHA256:        ssecKeySHA256,
		ServerSideEncryption: objectSSECAlgorithm(ssecUsed),
	})

	storageTier := common.TierHot
	if err := s.gateway.store.Put(r.Context(), bucket, key, dataReader, actualStorageSize, storageTier, objMetadata); err != nil {
		return fmt.Errorf("failed to store object: %w", err)
	}

	storedChecksum, storedChecksumType, checksumErr := checksumRecorder.Finalize()
	if checksumErr != nil {
		s.gateway.store.Delete(r.Context(), bucket, key, storageTier)
		if mismatch, ok := checksumErr.(*objectChecksumMismatch); ok {
			s.gateway.writeError(w, http.StatusBadRequest, "BadDigest", mismatch.clientMessage())
			return nil
		}
		return checksumErr
	}

	meta.Checksum = storedChecksum
	meta.ChecksumType = storedChecksumType

	if ssecUsed && s.gateway.config != nil && s.gateway.config.Encryption.EnableDedup {
	}

	if err := s.gateway.putObjectMetadataAfterStore(
		r.Context(),
		bucket,
		key,
		storageTier,
		meta,
	); err != nil {
		return err
	}

	if s.gateway.tiering != nil {
		s.gateway.tiering.RecordAccess(r.Context(), bucket, key, "PUT", userID)
	}

	if s.writeTasks != nil {
		s.writeTasks.ScheduleObjectWriteTasks(r.Context(), objectWriteTaskRequest{
			Bucket:        bucket,
			Key:           key,
			ContentType:   contentType,
			Metadata:      metadataMap,
			UserID:        userID,
			VersionID:     objMetadata.VersionID,
			SkipVectorize: r.Header.Get("X-Vectorize") == "false",
			SkipFTS:       r.Header.Get("X-FTS-Index") == "false",
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

	userID := s.gateway.requestUserID(r)

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
			writeObjectResponseHeaders(w, r, objMetadata, objectResponseHeaderOptions{
				ContentLength: int64(len(cachedData)),
				ETag:          etag,
				AcceptRanges:  true,
				CacheControl:  "max-age=3600",
				CacheStatus:   "HIT",
			})
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
		clientKey, ok := s.validateObjectSSECKey(w, r, objMetadata, "Invalid SSE-C customer key headers")
		if !ok {
			return nil
		}
		if s.gateway.cryptoCoordinator != nil && len(objMetadata.EncryptedDEK) > 0 {
			decryptedReader, err := s.gateway.cryptoCoordinator.DecryptWithClientKey(r.Context(), reader, clientKey, objMetadata.EncryptedDEK, objMetadata.Size)
			zeroClientKey(clientKey)
			if err != nil {
				s.gateway.writeError(w, http.StatusInternalServerError, "InternalError", "An internal error occurred while processing the object")
				return nil
			}
			dataReader = decryptedReader
		} else {
			zeroClientKey(clientKey)
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

	var statusCode = http.StatusOK
	var contentRange string

	if rangeHeader != "" {
		start, end, err := parseRangeHeader(rangeHeader, objMetadata.Size)
		if err != nil {
			s.gateway.writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", err.Error())
			return nil
		}

		if seeker, ok := dataReader.(io.Seeker); ok {
			if _, err := seeker.Seek(start, io.SeekStart); err != nil {
				return fmt.Errorf("failed to seek object reader to range start: %w", err)
			}
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

	cacheStatus := ""
	if s.gateway.objectCache != nil {
		cacheStatus = "MISS"
	}
	writeObjectResponseHeaders(w, r, objMetadata, objectResponseHeaderOptions{
		ContentLength: contentLength,
		ETag:          etag,
		AcceptRanges:  true,
		CacheControl:  "max-age=3600",
		CacheStatus:   cacheStatus,
		ContentRange:  contentRange,
	})

	w.WriteHeader(statusCode)

	if s.gateway.objectCache != nil && objMetadata.Size < 1<<20 && rangeHeader == "" && !objMetadata.Encrypted {
		data, err := io.ReadAll(io.LimitReader(dataReader, 1<<20))
		if err == nil {
			s.gateway.objectCache.Set(r.Context(), cacheKey, data)
			if _, writeErr := w.Write(data); writeErr != nil {
				if isClientDisconnected(writeErr) {
					return nil
				}
				return fmt.Errorf("failed to write cached object body: %w", writeErr)
			}
			return nil
		}
	}

	if _, err := io.Copy(w, dataReader); err != nil {
		if isClientDisconnected(err) {
			return nil
		}
		return fmt.Errorf("failed to stream object body: %w", err)
	}

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
		clientKey, ok := s.validateObjectSSECKey(w, r, objMetadata, "Invalid SSE-C customer key headers")
		if !ok {
			return nil
		}
		zeroClientKey(clientKey)
	}

	writeObjectResponseHeaders(w, r, objMetadata, objectResponseHeaderOptions{
		ContentLength:       objMetadata.Size,
		IncludeStorageClass: true,
	})

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
		if err := s.gateway.store.Delete(r.Context(), bucket, key, storageTier); err != nil {
			return fmt.Errorf("failed to delete object data: %w", err)
		}

		if s.gateway.vector != nil {
			s.gateway.vector.DeleteVector(r.Context(), bucket, key)
		}

		if s.gateway.ftsIndex != nil {
			s.gateway.ftsIndex.DeleteDocumentByKey(bucket, key)
		}

		if s.gateway.objectCache != nil {
			s.gateway.objectCache.Delete(r.Context(), bucket+"/"+key)
		}

		if err := s.gateway.metadata.DeleteObject(r.Context(), bucket, key); err != nil {
			return fmt.Errorf("failed to delete object metadata: %w", err)
		}
	}

	userID := s.gateway.requestUserID(r)
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

	userID := s.gateway.requestUserID(r)

	result := DeleteObjectsResult{}

	for _, obj := range req.Objects {
		objMetadata, err := s.gateway.metadata.GetObject(r.Context(), bucket, obj.Key)
		if err != nil {
			result.Error = append(result.Error, DeleteObjectsError{
				Key:     obj.Key,
				Code:    "NoSuchKey",
				Message: fmt.Sprintf("Object '%s' not found", obj.Key),
			})
			continue
		}

		storageTier := common.StorageTier(objMetadata.StorageTier)
		if err := s.gateway.store.Delete(r.Context(), bucket, obj.Key, storageTier); err != nil {
			result.Error = append(result.Error, DeleteObjectsError{
				Key:     obj.Key,
				Code:    "InternalError",
				Message: fmt.Sprintf("Failed to delete object data: %v", err),
			})
			continue
		}

		if s.gateway.vector != nil {
			s.gateway.vector.DeleteVector(r.Context(), bucket, obj.Key)
		}

		if s.gateway.ftsIndex != nil {
			s.gateway.ftsIndex.DeleteDocumentByKey(bucket, obj.Key)
		}

		if s.gateway.objectCache != nil {
			s.gateway.objectCache.Delete(r.Context(), bucket+"/"+obj.Key)
		}

		if err := s.gateway.metadata.DeleteObject(r.Context(), bucket, obj.Key); err != nil {
			result.Error = append(result.Error, DeleteObjectsError{
				Key:     obj.Key,
				Code:    "InternalError",
				Message: fmt.Sprintf("Failed to delete object metadata: %v", err),
			})
			continue
		}

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

		if !req.Quiet {
			result.Deleted = append(result.Deleted, DeleteObjectsDeleted{
				Key:       obj.Key,
				VersionID: objMetadata.VersionID,
			})
		}
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

	userID := s.gateway.requestUserID(r)

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
	for k, v := range parseUserMetadata(r.Header) {
		metadataMap[k] = v
	}

	contentType := srcMeta.ContentType
	if ct := r.Header.Get("Content-Type"); ct != "" {
		contentType = ct
	}

	etag := uuid.New().String()
	versionID := uuid.New().String()

	objMetadata, meta := buildActiveObjectMetadata(activeObjectMetadataInput{
		Bucket:       bucket,
		Key:          key,
		Size:         srcMeta.Size,
		ContentType:  contentType,
		ETag:         etag,
		UserMetadata: metadataMap,
		StorageTier:  common.TierHot,
		Encrypted:    srcMeta.Encrypted,
		VersionID:    versionID,
	})

	storageTier := common.TierHot

	if err := s.gateway.putStoredObjectWithMetadata(
		r.Context(),
		bucket,
		key,
		srcReader,
		srcMeta.Size,
		storageTier,
		objMetadata,
		meta,
		"failed to copy object",
	); err != nil {
		return err
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
