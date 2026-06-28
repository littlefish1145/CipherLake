package s3

import (
	"time"
)

// ObjectInfo contains metadata about an S3 object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	ContentType  string
	Metadata     map[string]string
}

// BucketInfo contains metadata about an S3 bucket.
type BucketInfo struct {
	Name         string
	CreationDate time.Time
}

// SearchResult represents a vector search result.
type SearchResult struct {
	Key       string
	Bucket    string
	Score     float64
	Metadata  map[string]string
}

// FTSResult represents a full-text search result.
type FTSResult struct {
	Key      string
	Bucket   string
	Score    float64
	Snippet  string
	Metadata map[string]string
}

// ListObjectsResult contains the result of a list objects operation.
type ListObjectsResult struct {
	Objects     []ObjectInfo
	Prefixes    []string
	IsTruncated bool
	NextToken   string
}
