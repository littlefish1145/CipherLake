package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type ObjectReader interface {
	ReadAt(ctx context.Context, buf []byte, offset int64) (int, error)
	Size(ctx context.Context) (int64, error)
	Close() error
}

type LocalReader struct {
	path string
	file *os.File
	size int64
}

func NewLocalReader(path string) (*LocalReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("local reader open %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("local reader stat %s: %w", path, err)
	}
	return &LocalReader{path: path, file: f, size: fi.Size()}, nil
}

func (r *LocalReader) ReadAt(_ context.Context, buf []byte, offset int64) (int, error) {
	return r.file.ReadAt(buf, offset)
}

func (r *LocalReader) Size(_ context.Context) (int64, error) {
	return r.size, nil
}

func (r *LocalReader) Close() error {
	return r.file.Close()
}

type S3Reader struct {
	endpoint string
	bucket   string
	key      string
	region   string
	size     int64
	client   *http.Client
}

type S3Config struct {
	Endpoint string
	Region   string
	Bucket   string
	AccessKey string
	SecretKey string
}

func NewS3Reader(cfg *S3Config, key string) (*S3Reader, error) {
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	client := &http.Client{}

	s3r := &S3Reader{
		endpoint: endpoint,
		bucket:   cfg.Bucket,
		key:      key,
		region:   cfg.Region,
		client:   client,
	}

	ctx := context.Background()
	size, err := s3r.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("s3 reader get size: %w", err)
	}
	s3r.size = size
	return s3r, nil
}

func (r *S3Reader) buildURL() string {
	return fmt.Sprintf("%s/%s/%s", r.endpoint, r.bucket, r.key)
}

func (r *S3Reader) ReadAt(ctx context.Context, buf []byte, offset int64) (int, error) {
	end := offset + int64(len(buf)) - 1
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.buildURL(), nil)
	if err != nil {
		return 0, fmt.Errorf("s3 request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("s3 get range: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("s3 range request failed: %s", resp.Status)
	}

	total := 0
	for total < len(buf) {
		n, err := resp.Body.Read(buf[total:])
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, fmt.Errorf("s3 read body: %w", err)
		}
	}
	return total, nil
}

func (r *S3Reader) Size(_ context.Context) (int64, error) {
	if r.size > 0 {
		return r.size, nil
	}

	req, err := http.NewRequest(http.MethodHead, r.buildURL(), nil)
	if err != nil {
		return 0, fmt.Errorf("s3 head request: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("s3 head: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("s3 head failed: %s", resp.Status)
	}

	size := resp.ContentLength
	if size <= 0 {
		return 0, fmt.Errorf("s3 head invalid content-length")
	}
	return size, nil
}

func (r *S3Reader) Close() error {
	r.client.CloseIdleConnections()
	return nil
}

type StoreType int8

const (
	StoreLocal StoreType = iota
	StoreS3
)

type ObjectStore struct {
	Type   StoreType
	Local  *LocalReader
	S3     *S3Reader
}

func (s *ObjectStore) ReadAt(ctx context.Context, buf []byte, offset int64) (int, error) {
	switch s.Type {
	case StoreLocal:
		return s.Local.ReadAt(ctx, buf, offset)
	case StoreS3:
		return s.S3.ReadAt(ctx, buf, offset)
	default:
		return 0, fmt.Errorf("unknown store type")
	}
}

func (s *ObjectStore) Size(ctx context.Context) (int64, error) {
	switch s.Type {
	case StoreLocal:
		return s.Local.Size(ctx)
	case StoreS3:
		return s.S3.Size(ctx)
	default:
		return 0, fmt.Errorf("unknown store type")
	}
}

func (s *ObjectStore) Close() error {
	switch s.Type {
	case StoreLocal:
		return s.Local.Close()
	case StoreS3:
		return s.S3.Close()
	}
	return nil
}
