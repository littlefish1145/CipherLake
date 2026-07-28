package gateway

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"cipherlake/internal/metadata"

	"github.com/google/uuid"
)

type BucketService struct {
	gateway *S3Gateway
}

func NewBucketService(gateway *S3Gateway) *BucketService {
	return &BucketService{gateway: gateway}
}

func (s *BucketService) handleListBuckets(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "GET" {
		return fmt.Errorf("method not allowed")
	}

	buckets, err := s.gateway.metadata.ListBuckets(r.Context())
	if err != nil {
		return fmt.Errorf("failed to list buckets: %w", err)
	}

	output := ListBucketsOutput{}
	output.Owner.ID = defaultOwnerID
	output.Owner.DisplayName = defaultOwnerDisplayName
	output.Buckets = make([]struct {
		Bucket struct {
			Name         string    `xml:"Name"`
			CreationDate time.Time `xml:"CreationDate"`
		} `xml:"Bucket"`
	}, len(buckets))

	for i, b := range buckets {
		output.Buckets[i].Bucket.Name = b.Name
		output.Buckets[i].Bucket.CreationDate = b.CreatedAt
	}

	return s.gateway.writeXML(w, http.StatusOK, output)
}

func (s *BucketService) handleCreateBucket(w http.ResponseWriter, r *http.Request, bucket string) error {
	if r.Method != "PUT" {
		return fmt.Errorf("method not allowed")
	}

	acl := r.Header.Get("x-amz-acl")
	if acl == "" {
		acl = "private"
	}

	userID := s.gateway.getUserID(r)
	if userID == "" || userID == "anonymous" {
		userID = "admin"
	}

	region := "us-east-1"
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err == nil && len(body) > 0 {
		type CreateBucketConfiguration struct {
			LocationConstraint string `xml:"LocationConstraint"`
		}
		var config CreateBucketConfiguration
		if err := xml.Unmarshal(body, &config); err == nil && config.LocationConstraint != "" {
			region = config.LocationConstraint
		}
	}

	bucketInfo := &metadata.BucketInfo{
		Name:      bucket,
		CreatedAt: time.Now(),
		OwnerID:   userID,
		OwnerName: userID,
		Region:    region,
		ACL:       acl,
	}

	if err := s.gateway.metadata.CreateBucket(r.Context(), bucket, bucketInfo); err != nil {
		return fmt.Errorf("failed to create bucket: %w", err)
	}

	w.Header().Set("Location", "/"+bucket)
	w.Header().Set("x-amz-request-id", uuid.New().String())
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *BucketService) handleDeleteBucket(w http.ResponseWriter, r *http.Request, bucket string) error {
	if r.Method != "DELETE" {
		return fmt.Errorf("method not allowed")
	}

	// S3 semantics: deleting a non-existent bucket returns 404 NoSuchBucket,
	// and deleting a non-empty bucket returns 409 BucketNotEmpty. The
	// metadata layer's DeleteBucket cascades deletes and treats missing
	// buckets as a no-op for internal/admin callers, so the gateway must
	// enforce the public S3 contract here.
	if _, err := s.gateway.metadata.GetBucket(r.Context(), bucket); err != nil {
		return fmt.Errorf("failed to delete bucket: %w", err)
	}

	objects, err := s.gateway.metadata.ListObjects(r.Context(), bucket, "", 1)
	if err != nil {
		return fmt.Errorf("failed to list objects for bucket emptiness check: %w", err)
	}
	if len(objects) > 0 {
		return fmt.Errorf("%w: bucket %s still has objects", metadata.ErrBucketNotEmpty, bucket)
	}

	uploads, _ := s.gateway.metadata.ListUploads(r.Context(), bucket)
	if len(uploads) > 0 {
		return fmt.Errorf("%w: bucket %s still has in-progress multipart uploads", metadata.ErrBucketNotEmpty, bucket)
	}

	if err := s.gateway.metadata.DeleteBucket(r.Context(), bucket); err != nil {
		return fmt.Errorf("failed to delete bucket: %w", err)
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *BucketService) handleHeadBucket(w http.ResponseWriter, r *http.Request, bucket string) error {
	if r.Method != "HEAD" {
		return fmt.Errorf("method not allowed")
	}

	_, err := s.gateway.metadata.GetBucket(r.Context(), bucket)
	if err != nil {
		return fmt.Errorf("bucket not found: %w", err)
	}

	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *BucketService) handleListObjects(w http.ResponseWriter, r *http.Request, bucket string) error {
	if r.Method != "GET" {
		return fmt.Errorf("method not allowed")
	}

	query := r.URL.Query()
	prefix := query.Get("prefix")
	delimiter := query.Get("delimiter")
	maxKeysStr := query.Get("max-keys")
	listType := query.Get("list-type")

	maxKeys := 1000
	if maxKeysStr != "" {
		if m, err := strconv.Atoi(maxKeysStr); err == nil {
			maxKeys = m
		}
	}

	if listType == "2" {
		return s.handleListObjectsV2(w, r, bucket, prefix, delimiter, maxKeys, query)
	}

	marker := query.Get("marker")
	objects, err := s.gateway.metadata.ListObjects(r.Context(), bucket, prefix, maxKeys)
	if err != nil {
		return fmt.Errorf("failed to list objects: %w", err)
	}

	output := ListObjectsOutput{
		Name:        bucket,
		Prefix:      prefix,
		Marker:      marker,
		MaxKeys:     maxKeys,
		Delimiter:   delimiter,
		IsTruncated: len(objects) == maxKeys,
	}

	for _, obj := range objects {
		if commonPrefix, ok := commonPrefixForObject(obj.Key, prefix, delimiter); ok {
			output.CommonPrefixes = append(output.CommonPrefixes, struct {
				Prefix string `xml:"Prefix"`
			}{Prefix: commonPrefix})
			continue
		}

		output.Contents = append(output.Contents, objectListContent(obj))
	}

	if output.IsTruncated && len(output.Contents) > 0 {
		output.NextMarker = output.Contents[len(output.Contents)-1].Key
	}

	return s.gateway.writeXML(w, http.StatusOK, output)
}

func (s *BucketService) handleListObjectsV2(w http.ResponseWriter, r *http.Request, bucket, prefix, delimiter string, maxKeys int, query map[string][]string) error {
	startAfter := ""
	if v, ok := query["start-after"]; ok && len(v) > 0 {
		startAfter = v[0]
	}
	continuationToken := ""
	if v, ok := query["continuation-token"]; ok && len(v) > 0 {
		continuationToken = v[0]
	}

	objects, err := s.gateway.metadata.ListObjects(r.Context(), bucket, prefix, maxKeys+1)
	if err != nil {
		return fmt.Errorf("failed to list objects: %w", err)
	}

	if startAfter != "" {
		filtered := make([]*metadata.ObjectMetadata, 0, len(objects))
		passedStart := false
		for _, obj := range objects {
			if obj.Key == startAfter {
				passedStart = true
				continue
			}
			if passedStart {
				filtered = append(filtered, obj)
			}
		}
		objects = filtered
	} else if continuationToken != "" {
		filtered := make([]*metadata.ObjectMetadata, 0, len(objects))
		passedToken := false
		for _, obj := range objects {
			if obj.Key == continuationToken {
				passedToken = true
				continue
			}
			if passedToken {
				filtered = append(filtered, obj)
			}
		}
		objects = filtered
	}

	isTruncated := len(objects) > maxKeys
	if isTruncated {
		objects = objects[:maxKeys]
	}

	output := ListObjectsV2Output{
		Name:              bucket,
		Prefix:            prefix,
		StartAfter:        startAfter,
		ContinuationToken: continuationToken,
		KeyCount:          len(objects),
		MaxKeys:           maxKeys,
		Delimiter:         delimiter,
		IsTruncated:       isTruncated,
	}

	for _, obj := range objects {
		if commonPrefix, ok := commonPrefixForObject(obj.Key, prefix, delimiter); ok {
			output.CommonPrefixes = append(output.CommonPrefixes, struct {
				Prefix string `xml:"Prefix"`
			}{Prefix: commonPrefix})
			continue
		}

		output.Contents = append(output.Contents, objectListContent(obj))
	}

	if isTruncated && len(output.Contents) > 0 {
		output.NextContinuationToken = output.Contents[len(output.Contents)-1].Key
	}

	return s.gateway.writeXML(w, http.StatusOK, output)
}

func (s *BucketService) handleGetBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) error {
	type LocationConstraint struct {
		XMLName  xml.Name `xml:"LocationConstraint"`
		Location string   `xml:",chardata"`
	}

	bucketInfo, err := s.gateway.metadata.GetBucket(r.Context(), bucket)
	if err != nil {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return nil
	}

	location := bucketInfo.Region
	if location == "" {
		location = "us-east-1"
	}

	output := struct {
		XMLName            xml.Name `xml:"LocationConstraint"`
		LocationConstraint string   `xml:",chardata"`
	}{
		LocationConstraint: location,
	}

	if location == "us-east-1" {
		output.LocationConstraint = ""
	}

	return s.gateway.writeXML(w, http.StatusOK, output)
}

func (s *BucketService) handleGetBucketAcl(w http.ResponseWriter, r *http.Request, bucket string) error {
	bucketInfo, err := s.gateway.metadata.GetBucket(r.Context(), bucket)
	if err != nil {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return nil
	}

	acl := bucketInfo.ACL
	if acl == "" {
		acl = "private"
	}

	ownerID := bucketInfo.OwnerID
	if ownerID == "" {
		ownerID = "admin"
	}
	ownerName := bucketInfo.OwnerName
	if ownerName == "" {
		ownerName = "admin"
	}

	type Grant struct {
		Grantee struct {
			XMLName     xml.Name `xml:"Grantee"`
			XMLNSXSI    string   `xml:"xmlns:xsi,attr"`
			XSIType     string   `xml:"xsi:type,attr"`
			Type        string   `xml:"Type"`
			ID          string   `xml:"ID"`
			DisplayName string   `xml:"DisplayName,omitempty"`
		} `xml:"Grant"`
		Permission string `xml:"Permission"`
	}

	type AccessControlPolicy struct {
		XMLName xml.Name `xml:"AccessControlPolicy"`
		Owner   struct {
			ID          string `xml:"ID"`
			DisplayName string `xml:"DisplayName"`
		} `xml:"Owner"`
		AccessControlList struct {
			Grants []Grant `xml:"Grant"`
		} `xml:"AccessControlList"`
	}

	policy := AccessControlPolicy{}
	policy.Owner.ID = ownerID
	policy.Owner.DisplayName = ownerName

	fullControlGrant := Grant{}
	fullControlGrant.Grantee.XMLNSXSI = "http://www.w3.org/2001/XMLSchema-instance"
	fullControlGrant.Grantee.XSIType = "CanonicalUser"
	fullControlGrant.Grantee.Type = "CanonicalUser"
	fullControlGrant.Grantee.ID = ownerID
	fullControlGrant.Grantee.DisplayName = ownerName
	fullControlGrant.Permission = "FULL_CONTROL"
	policy.AccessControlList.Grants = append(policy.AccessControlList.Grants, fullControlGrant)

	if acl == "public-read" || acl == "public-read-write" {
		publicReadGrant := Grant{}
		publicReadGrant.Grantee.XMLNSXSI = "http://www.w3.org/2001/XMLSchema-instance"
		publicReadGrant.Grantee.XSIType = "Group"
		publicReadGrant.Grantee.Type = "Group"
		publicReadGrant.Grantee.ID = "http://acs.amazonaws.com/groups/global/AllUsers"
		publicReadGrant.Permission = "READ"
		policy.AccessControlList.Grants = append(policy.AccessControlList.Grants, publicReadGrant)
	}

	if acl == "public-read-write" {
		publicWriteGrant := Grant{}
		publicWriteGrant.Grantee.XMLNSXSI = "http://www.w3.org/2001/XMLSchema-instance"
		publicWriteGrant.Grantee.XSIType = "Group"
		publicWriteGrant.Grantee.Type = "Group"
		publicWriteGrant.Grantee.ID = "http://acs.amazonaws.com/groups/global/AllUsers"
		publicWriteGrant.Permission = "WRITE"
		policy.AccessControlList.Grants = append(policy.AccessControlList.Grants, publicWriteGrant)
	}

	return s.gateway.writeXML(w, http.StatusOK, policy)
}

func (s *BucketService) handlePutBucketAcl(w http.ResponseWriter, r *http.Request, bucket string) error {
	bucketInfo, err := s.gateway.metadata.GetBucket(r.Context(), bucket)
	if err != nil {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return nil
	}

	acl := r.Header.Get("x-amz-acl")
	if acl == "" {
		acl = r.URL.Query().Get("acl")
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err == nil && len(body) > 0 {
		type AccessControlPolicy struct {
			AccessControlList struct {
				Grants []struct {
					Grantee struct {
						Type string `xml:"Type"`
						ID   string `xml:"ID"`
					} `xml:"Grantee"`
					Permission string `xml:"Permission"`
				} `xml:"Grant"`
			} `xml:"AccessControlList"`
		}

		var policy AccessControlPolicy
		if err := xml.Unmarshal(body, &policy); err == nil {
			hasPublicRead := false
			hasPublicWrite := false
			for _, grant := range policy.AccessControlList.Grants {
				if grant.Grantee.Type == "Group" && grant.Grantee.ID == "http://acs.amazonaws.com/groups/global/AllUsers" {
					if grant.Permission == "READ" {
						hasPublicRead = true
					}
					if grant.Permission == "WRITE" {
						hasPublicWrite = true
					}
				}
			}
			if hasPublicRead && hasPublicWrite {
				acl = "public-read-write"
			} else if hasPublicRead {
				acl = "public-read"
			} else {
				acl = "private"
			}
		}
	}

	validACLs := map[string]bool{
		"private":            true,
		"public-read":        true,
		"public-read-write":  true,
		"authenticated-read": true,
	}
	if !validACLs[acl] {
		acl = "private"
	}

	bucketInfo.ACL = acl
	if err := s.gateway.metadata.UpdateBucket(r.Context(), bucket, bucketInfo); err != nil {
		return fmt.Errorf("failed to update bucket ACL: %w", err)
	}

	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *BucketService) handleGetBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) error {
	_, err := s.gateway.metadata.GetBucket(r.Context(), bucket)
	if err != nil {
		s.gateway.writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return nil
	}

	output := struct {
		XMLName   xml.Name `xml:"VersioningConfiguration"`
		Status    string   `xml:"Status,omitempty"`
		MFADelete string   `xml:"MfaDelete,omitempty"`
	}{}

	return s.gateway.writeXML(w, http.StatusOK, output)
}
