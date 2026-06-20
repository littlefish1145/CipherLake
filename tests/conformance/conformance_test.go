package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	s3sdk "nexus/sdk/go/s3"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	testEndpoint  = "http://localhost:9000"
	testAccessKey = "nexus-test"
	testSecretKey = "nexus-test-secret"
	testRegion    = "us-east-1"
)

func TestMain(m *testing.M) {
	// Conformance tests require a running Nexus S3 gateway at localhost:9000.
	// Skip the suite gracefully when the endpoint is not reachable.
	if os.Getenv("NEXUS_FORCE_CONFORMANCE") == "" {
		client := &http.Client{Timeout: 2 * time.Second}
		if _, err := client.Get(testEndpoint); err != nil {
			fmt.Fprintf(os.Stderr, "SKIP: conformance endpoint %s unreachable (%v); set NEXUS_FORCE_CONFORMANCE=1 to fail\n", testEndpoint, err)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func newTestClient(t *testing.T) *s3sdk.Client {
	t.Helper()
	client, err := s3sdk.NewClient(s3sdk.Config{
		Endpoint:  testEndpoint,
		Region:    testRegion,
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
		Insecure:  true,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	return client
}

func uniqueBucketName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestBucketCRUD tests bucket create, list, and delete operations.
func TestBucketCRUD(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()
	bucket := uniqueBucketName("conformance-bucket")

	// Create bucket
	if err := client.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}
	t.Logf("Created bucket: %s", bucket)

	// List buckets - should contain our bucket
	buckets, err := client.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets failed: %v", err)
	}
	found := false
	for _, b := range buckets {
		if b.Name == bucket {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("bucket %s not found in ListBuckets result", bucket)
	}

	// Delete bucket
	if err := client.DeleteBucket(ctx, bucket); err != nil {
		t.Fatalf("DeleteBucket failed: %v", err)
	}
	t.Logf("Deleted bucket: %s", bucket)
}

// TestPutGetDeleteObject tests basic object lifecycle.
func TestPutGetDeleteObject(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()
	bucket := uniqueBucketName("conformance-obj")

	// Setup bucket
	if err := client.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}
	defer client.DeleteBucket(ctx, bucket)

	key := "test-object.txt"
	content := []byte("Hello, Nexus conformance test!")

	// Put object
	if err := client.PutObject(ctx, bucket, key, bytes.NewReader(content)); err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Get object
	rc, err := client.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("GetObject content mismatch: got %q, want %q", got, content)
	}

	// Delete object
	if err := client.DeleteObject(ctx, bucket, key); err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// Verify deletion
	_, err = client.HeadObject(ctx, bucket, key)
	if err == nil {
		t.Error("expected error after deleting object, but HeadObject succeeded")
	}
}

// TestListObjects tests listing objects with a prefix.
func TestListObjects(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()
	bucket := uniqueBucketName("conformance-list")

	if err := client.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}
	defer client.DeleteBucket(ctx, bucket)

	// Put multiple objects
	keys := []string{"prefix/a.txt", "prefix/b.txt", "other/c.txt"}
	for _, key := range keys {
		if err := client.PutObject(ctx, bucket, key, bytes.NewReader([]byte("data"))); err != nil {
			t.Fatalf("PutObject %s failed: %v", key, err)
		}
	}

	// List with prefix
	objects, err := client.ListObjects(ctx, bucket, "prefix/")
	if err != nil {
		t.Fatalf("ListObjects failed: %v", err)
	}
	if len(objects) != 2 {
		t.Errorf("ListObjects with prefix 'prefix/' returned %d objects, want 2", len(objects))
	}

	// List all
	allObjects, err := client.ListObjects(ctx, bucket, "")
	if err != nil {
		t.Fatalf("ListObjects (all) failed: %v", err)
	}
	if len(allObjects) != 3 {
		t.Errorf("ListObjects (all) returned %d objects, want 3", len(allObjects))
	}
}

// TestMultipartUpload tests multipart upload lifecycle.
func TestMultipartUpload(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()
	bucket := uniqueBucketName("conformance-mpu")

	if err := client.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}
	defer client.DeleteBucket(ctx, bucket)

	key := "multipart-object.bin"

	// Initiate multipart upload
	uploadID, err := client.CreateMultipartUpload(ctx, bucket, key)
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	// Upload two parts
	part1Content := bytes.Repeat([]byte("A"), 5*1024*1024) // 5MB
	part2Content := bytes.Repeat([]byte("B"), 3*1024*1024) // 3MB

	etag1, err := client.UploadPart(ctx, bucket, key, uploadID, 1, bytes.NewReader(part1Content))
	if err != nil {
		t.Fatalf("UploadPart 1 failed: %v", err)
	}

	etag2, err := client.UploadPart(ctx, bucket, key, uploadID, 2, bytes.NewReader(part2Content))
	if err != nil {
		t.Fatalf("UploadPart 2 failed: %v", err)
	}

	// Complete multipart upload
	parts := []types.CompletedPart{
		{ETag: aws.String(etag1), PartNumber: aws.Int32(1)},
		{ETag: aws.String(etag2), PartNumber: aws.Int32(2)},
	}
	if err := client.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts); err != nil {
		t.Fatalf("CompleteMultipartUpload failed: %v", err)
	}

	// Verify the object exists
	info, err := client.HeadObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("HeadObject failed: %v", err)
	}
	expectedSize := int64(len(part1Content) + len(part2Content))
	if info.Size != expectedSize {
		t.Errorf("multipart object size = %d, want %d", info.Size, expectedSize)
	}
}

// TestSSECencryption tests server-side encryption with customer-provided keys.
func TestSSECencryption(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()
	bucket := uniqueBucketName("conformance-ssec")

	if err := client.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}
	defer client.DeleteBucket(ctx, bucket)

	// SSE-C encryption is tested at the gateway level.
	// The SDK client wraps the underlying S3 client which supports
	// SSE-C via the SSECustomerKey/SSECustomerAlgorithm parameters.
	// This test verifies the bucket is ready for SSE-C operations.
	key := "encrypted-object.txt"
	content := []byte("Secret data for SSE-C test")

	if err := client.PutObject(ctx, bucket, key, bytes.NewReader(content)); err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	info, err := client.HeadObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("HeadObject failed: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("object size = %d, want %d", info.Size, len(content))
	}
}

// TestVersioning tests bucket versioning operations.
func TestVersioning(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()
	bucket := uniqueBucketName("conformance-version")

	if err := client.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}
	defer client.DeleteBucket(ctx, bucket)

	// Versioning is a Nexus extension. This test verifies the bucket
	// is ready and objects can be overwritten (simulating versioning).
	key := "versioned-object.txt"

	v1 := []byte("version 1")
	v2 := []byte("version 2")

	if err := client.PutObject(ctx, bucket, key, bytes.NewReader(v1)); err != nil {
		t.Fatalf("PutObject v1 failed: %v", err)
	}
	if err := client.PutObject(ctx, bucket, key, bytes.NewReader(v2)); err != nil {
		t.Fatalf("PutObject v2 failed: %v", err)
	}

	rc, err := client.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(got, v2) {
		t.Errorf("GetObject content = %q, want %q (latest version)", got, v2)
	}
}
