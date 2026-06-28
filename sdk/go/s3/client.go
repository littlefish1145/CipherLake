package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Client wraps the AWS S3 client with Nexus-specific extensions.
type Client struct {
	client *s3.Client
}

// Config holds the configuration for creating a new S3-compatible Client.
type Config struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	Insecure  bool
}

// NewClient creates a new S3-compatible client configured for Nexus.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	s3Opts := s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle: true,
	}

	if cfg.Insecure {
		s3Opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: nil, // uses default, caller should customize for insecure
			},
		}
	}

	s3Client := s3.New(s3Opts)

	return &Client{client: s3Client}, nil
}

// PutObject uploads an object to the specified bucket.
func (c *Client) PutObject(ctx context.Context, bucket, key string, body io.Reader) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   body,
	})
	if err != nil {
		return fmt.Errorf("put object %s/%s: %w", bucket, key, err)
	}
	return nil
}

// GetObject retrieves an object from the specified bucket.
func (c *Client) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	resp, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %s/%s: %w", bucket, key, err)
	}
	return resp.Body, nil
}

// DeleteObject deletes an object from the specified bucket.
func (c *Client) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object %s/%s: %w", bucket, key, err)
	}
	return nil
}

// ListObjects lists objects in the specified bucket with an optional prefix.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}

	var objects []ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(c.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects in %s: %w", bucket, err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, ObjectInfo{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
				ETag:         aws.ToString(obj.ETag),
			})
		}
	}
	return objects, nil
}

// HeadObject retrieves metadata for an object without downloading it.
func (c *Client) HeadObject(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	resp, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("head object %s/%s: %w", bucket, key, err)
	}

	info := &ObjectInfo{
		Key:         key,
		Size:        aws.ToInt64(resp.ContentLength),
		LastModified: aws.ToTime(resp.LastModified),
		ETag:        aws.ToString(resp.ETag),
		ContentType: aws.ToString(resp.ContentType),
		Metadata:    make(map[string]string),
	}
	for k, v := range resp.Metadata {
		info.Metadata[k] = v
	}
	return info, nil
}

// CreateBucket creates a new bucket.
func (c *Client) CreateBucket(ctx context.Context, bucket string) error {
	_, err := c.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	return nil
}

// DeleteBucket deletes an empty bucket.
func (c *Client) DeleteBucket(ctx context.Context, bucket string) error {
	_, err := c.client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return fmt.Errorf("delete bucket %s: %w", bucket, err)
	}
	return nil
}

// ListBuckets lists all buckets.
func (c *Client) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	resp, err := c.client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}

	var buckets []BucketInfo
	for _, b := range resp.Buckets {
		buckets = append(buckets, BucketInfo{
			Name:         aws.ToString(b.Name),
			CreationDate: aws.ToTime(b.CreationDate),
		})
	}
	return buckets, nil
}

// CopyObject copies an object from one location to another.
func (c *Client) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error {
	_, err := c.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(dstBucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(srcBucket + "/" + srcKey),
	})
	if err != nil {
		return fmt.Errorf("copy object %s/%s to %s/%s: %w", srcBucket, srcKey, dstBucket, dstKey, err)
	}
	return nil
}

// PutObjectWithMetadata uploads an object with custom metadata.
func (c *Client) PutObjectWithMetadata(ctx context.Context, bucket, key string, body io.Reader, contentType string, metadata map[string]string) error {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
		Metadata:    metadata,
	}
	_, err := c.client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("put object with metadata %s/%s: %w", bucket, key, err)
	}
	return nil
}

// CreateMultipartUpload initiates a multipart upload.
func (c *Client) CreateMultipartUpload(ctx context.Context, bucket, key string) (string, error) {
	resp, err := c.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", fmt.Errorf("create multipart upload %s/%s: %w", bucket, key, err)
	}
	return aws.ToString(resp.UploadId), nil
}

// UploadPart uploads a part in a multipart upload.
func (c *Client) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	resp, err := c.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(partNumber),
		Body:       body,
	})
	if err != nil {
		return "", fmt.Errorf("upload part %d for %s/%s: %w", partNumber, bucket, key, err)
	}
	return aws.ToString(resp.ETag), nil
}

// CompleteMultipartUpload completes a multipart upload.
func (c *Client) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []types.CompletedPart) error {
	_, err := c.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return fmt.Errorf("complete multipart upload %s/%s: %w", bucket, key, err)
	}
	return nil
}

// AbortMultipartUpload aborts a multipart upload.
func (c *Client) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	_, err := c.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		return fmt.Errorf("abort multipart upload %s/%s: %w", bucket, key, err)
	}
	return nil
}

// parseIntOrDefault parses a string as int or returns the default value.
func parseIntOrDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
