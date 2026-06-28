package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const s3MultipartThreshold = 8 * 1024 * 1024 // 8MB

type S3Config struct {
	Endpoint      string
	Region        string
	Bucket        string
	AccessKey     string
	SecretKey     string
	ForcePathStyle bool
}

type S3Backend struct {
	client *s3.Client
	uploader *manager.Uploader
	bucket string
}

func NewS3Backend(cfg S3Config) (*S3Backend, error) {
	var opts []func(*config.LoadOptions) error

	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}

	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	clientOpts := []func(*s3.Options){
		func(o *s3.Options) {
			o.UsePathStyle = cfg.ForcePathStyle
		},
	}

	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}

	client := s3.NewFromConfig(awsCfg, clientOpts...)

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = s3MultipartThreshold
		u.BufferProvider = manager.NewBufferedReadSeekerWriteToPool(s3MultipartThreshold)
	})

	return &S3Backend{
		client:   client,
		uploader: uploader,
		bucket:   cfg.Bucket,
	}, nil
}

func (s *S3Backend) Name() string {
	return "s3"
}

func (s *S3Backend) Put(ctx context.Context, path string, data io.Reader, size int64) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
		Body:   data,
	})
	if err != nil {
		return fmt.Errorf("failed to put object to S3: %w", err)
	}
	return nil
}

func (s *S3Backend) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}
	return resp.Body, nil
}

func (s *S3Backend) Delete(ctx context.Context, path string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object from S3: %w", err)
	}
	return nil
}

func (s *S3Backend) Exists(ctx context.Context, path string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to head object in S3: %w", err)
	}
	return true, nil
}

func (s *S3Backend) Size(ctx context.Context, path string) (int64, error) {
	resp, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return 0, ErrObjectNotFound
		}
		return 0, fmt.Errorf("failed to head object in S3: %w", err)
	}
	if resp.ContentLength != nil {
		return *resp.ContentLength, nil
	}
	return 0, nil
}

func (s *S3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects in S3: %w", err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, *obj.Key)
		}
	}
	return keys, nil
}

func (s *S3Backend) PutReader(ctx context.Context, path string, reader io.Reader) (string, error) {
	resp, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
		Body:   reader,
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload object to S3: %w", err)
	}
	etag := ""
	if resp.ETag != nil {
		etag = *resp.ETag
	}
	return etag, nil
}

func (s *S3Backend) GetRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("failed to get object range from S3: %w", err)
	}
	return resp.Body, nil
}

func (s *S3Backend) AtomicRename(ctx context.Context, oldPath, newPath string) error {
	// S3 doesn't support atomic rename natively; use copy + delete
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(newPath),
		CopySource: aws.String(s.bucket + "/" + oldPath),
	})
	if err != nil {
		return fmt.Errorf("failed to copy object in S3: %w", err)
	}

	// Verify the copy completed by checking the new object exists
	_, err = s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(newPath),
	})
	if err != nil {
		return fmt.Errorf("failed to verify copied object in S3: %w", err)
	}

	// Delete the old object
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(oldPath),
	})
	if err != nil {
		return fmt.Errorf("failed to delete old object after rename in S3: %w", err)
	}

	return nil
}

func (s *S3Backend) Close() error {
	// No-op for S3
	return nil
}

// parseS3Key extracts the key from a copy source string
func parseS3Key(copySource string) string {
	parts := strings.SplitN(copySource, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return copySource
}
