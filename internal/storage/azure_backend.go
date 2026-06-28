package storage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

type AzureConfig struct {
	AccountName string
	AccountKey  string
	Container   string
	Endpoint    string
}

type AzureBlobBackend struct {
	client       *azblob.Client
	containerURL string
	container    string
}

func NewAzureBlobBackend(cfg AzureConfig) (*AzureBlobBackend, error) {
	var client *azblob.Client
	var err error

	if cfg.Endpoint != "" {
		// Custom endpoint (e.g., Azurite or sovereign cloud)
		serviceURL := cfg.Endpoint
		if !strings.HasSuffix(serviceURL, "/") {
			serviceURL += "/"
		}
		cred, credErr := azblob.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
		if credErr != nil {
			return nil, fmt.Errorf("failed to create Azure shared key credential: %w", credErr)
		}
		client, err = azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	} else if cfg.AccountName != "" && cfg.AccountKey != "" {
		// Standard Azure endpoint
		serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.AccountName)
		cred, credErr := azblob.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
		if credErr != nil {
			return nil, fmt.Errorf("failed to create Azure shared key credential: %w", credErr)
		}
		client, err = azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	} else {
		return nil, fmt.Errorf("Azure storage account name and key are required")
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create Azure blob client: %w", err)
	}

	return &AzureBlobBackend{
		client:    client,
		container: cfg.Container,
	}, nil
}

func (a *AzureBlobBackend) Name() string {
	return "azure"
}

func (a *AzureBlobBackend) Put(ctx context.Context, path string, data io.Reader, size int64) error {
	_, err := a.client.UploadStream(ctx, a.container, path, data, nil)
	if err != nil {
		return fmt.Errorf("failed to upload blob to Azure: %w", err)
	}
	return nil
}

func (a *AzureBlobBackend) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := a.client.DownloadStream(ctx, a.container, path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to download blob from Azure: %w", err)
	}
	return resp.Body, nil
}

func (a *AzureBlobBackend) Delete(ctx context.Context, path string) error {
	_, err := a.client.DeleteBlob(ctx, a.container, path, nil)
	if err != nil {
		return fmt.Errorf("failed to delete blob from Azure: %w", err)
	}
	return nil
}

func (a *AzureBlobBackend) Exists(ctx context.Context, path string) (bool, error) {
	_, err := a.client.ServiceClient().NewContainerClient(a.container).NewBlobClient(path).GetProperties(ctx, nil)
	if err != nil {
		// Check if it's a "blob not found" error
		if isAzureBlobNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check blob existence in Azure: %w", err)
	}
	return true, nil
}

func (a *AzureBlobBackend) Size(ctx context.Context, path string) (int64, error) {
	props, err := a.client.ServiceClient().NewContainerClient(a.container).NewBlobClient(path).GetProperties(ctx, nil)
	if err != nil {
		if isAzureBlobNotFound(err) {
			return 0, ErrObjectNotFound
		}
		return 0, fmt.Errorf("failed to get blob properties from Azure: %w", err)
	}
	return *props.ContentLength, nil
}

func (a *AzureBlobBackend) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	pager := a.client.NewListBlobsFlatPager(a.container, &azblob.ListBlobsFlatOptions{
		Prefix: &prefix,
	})

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list blobs in Azure: %w", err)
		}
		for _, item := range page.Segment.BlobItems {
			keys = append(keys, *item.Name)
		}
	}
	return keys, nil
}

func (a *AzureBlobBackend) PutReader(ctx context.Context, path string, reader io.Reader) (string, error) {
	resp, err := a.client.UploadStream(ctx, a.container, path, reader, nil)
	if err != nil {
		return "", fmt.Errorf("failed to upload blob to Azure: %w", err)
	}
	etag := ""
	if resp.ETag != nil {
		etag = string(*resp.ETag)
	}
	return etag, nil
}

func (a *AzureBlobBackend) GetRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	resp, err := a.client.DownloadStream(ctx, a.container, path, &azblob.DownloadStreamOptions{
		Range: blob.HTTPRange{
			Offset: offset,
			Count:  length,
		},
	})
	if err != nil {
		if isAzureBlobNotFound(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("failed to download blob range from Azure: %w", err)
	}
	return resp.Body, nil
}

func (a *AzureBlobBackend) AtomicRename(ctx context.Context, oldPath, newPath string) error {
	// Azure Blob doesn't support atomic rename; use copy + delete
	srcURL := fmt.Sprintf("https://%s.blob.core.windows.net/%s/%s", a.container, a.container, oldPath)

	_, err := a.client.ServiceClient().NewContainerClient(a.container).NewBlobClient(newPath).CopyFromURL(ctx, srcURL, nil)
	if err != nil {
		return fmt.Errorf("failed to copy blob in Azure: %w", err)
	}

	_, err = a.client.DeleteBlob(ctx, a.container, oldPath, nil)
	if err != nil {
		return fmt.Errorf("failed to delete old blob after rename in Azure: %w", err)
	}

	return nil
}

func (a *AzureBlobBackend) Close() error {
	// No-op for Azure
	return nil
}

// isAzureBlobNotFound checks if the error indicates the blob was not found
func isAzureBlobNotFound(err error) bool {
	if err == nil {
		return false
	}
	// Azure SDK returns specific error types; check for common patterns
	errStr := err.Error()
	return strings.Contains(errStr, "StatusCode=404") ||
		strings.Contains(errStr, "BlobNotFound") ||
		strings.Contains(errStr, "The specified blob does not exist")
}

// Ensure container.Client is available for list operations
var _ *container.Client = nil
