package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// VectorSearch performs a vector similarity search across all indexed buckets.
// This is a Nexus-specific extension that uses the /_vector_search endpoint.
func (c *Client) VectorSearch(ctx context.Context, query string, topK int) ([]SearchResult, error) {
	if topK <= 0 {
		topK = 20
	}

	reqBody := map[string]interface{}{
		"query": query,
		"top_k": topK,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal vector search request: %w", err)
	}

	resp, err := c.doNexusRequest(ctx, http.MethodPost, "/_vector_search", body)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vector search failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Results []SearchResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode vector search response: %w", err)
	}
	return result.Results, nil
}

// FTSSearch performs a full-text search within a specific bucket.
// This is a Nexus-specific extension that uses the /{bucket}/_fts endpoint.
func (c *Client) FTSSearch(ctx context.Context, bucket, query string, topK int) ([]FTSResult, error) {
	if topK <= 0 {
		topK = 10
	}

	reqBody := map[string]interface{}{
		"query": query,
		"top_k": topK,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal FTS search request: %w", err)
	}

	path := fmt.Sprintf("/%s/_fts", bucket)
	resp, err := c.doNexusRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fts search failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Results []FTSResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode FTS search response: %w", err)
	}
	return result.Results, nil
}

// ResumableUpload performs a resumable upload for large files.
// This is a Nexus-specific extension that uses the resumable upload API.
func (c *Client) ResumableUpload(ctx context.Context, bucket, key string, reader io.Reader) error {
	// Step 1: Initiate resumable upload
	initBody := map[string]interface{}{
		"key": key,
	}
	initJSON, err := json.Marshal(initBody)
	if err != nil {
		return fmt.Errorf("marshal resumable init request: %w", err)
	}

	initPath := fmt.Sprintf("/%s/_resumable", bucket)
	resp, err := c.doNexusRequest(ctx, http.MethodPost, initPath, initJSON)
	if err != nil {
		return fmt.Errorf("initiate resumable upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("resumable upload init failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var initResult struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&initResult); err != nil {
		return fmt.Errorf("decode resumable init response: %w", err)
	}

	// Step 2: Upload data in chunks
	const chunkSize = 5 * 1024 * 1024 // 5MB chunks
	buf := make([]byte, chunkSize)
	partNumber := int32(1)

	for {
		n, err := io.ReadFull(reader, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return fmt.Errorf("read chunk: %w", err)
		}
		if n == 0 {
			break
		}

		// Upload part using standard multipart upload
		_, uploadErr := c.UploadPart(ctx, bucket, key, initResult.UploadID, partNumber, bytes.NewReader(buf[:n]))
		if uploadErr != nil {
			// Attempt to abort on failure
			_ = c.AbortMultipartUpload(ctx, bucket, key, initResult.UploadID)
			return fmt.Errorf("upload part %d: %w", partNumber, uploadErr)
		}
		partNumber++
	}

	// Step 3: Complete the upload
	parts := make([]struct {
		PartNumber int32
	}, 0, partNumber-1)
	for i := int32(1); i < partNumber; i++ {
		parts = append(parts, struct{ PartNumber int32 }{PartNumber: i})
	}

	completePath := fmt.Sprintf("/%s/_resumable/%s", bucket, initResult.UploadID)
	completeBody := map[string]interface{}{
		"key":       key,
		"upload_id": initResult.UploadID,
		"parts":     partNumber - 1,
	}
	completeJSON, _ := json.Marshal(completeBody)

	completeResp, err := c.doNexusRequest(ctx, http.MethodPut, completePath, completeJSON)
	if err != nil {
		return fmt.Errorf("complete resumable upload: %w", err)
	}
	defer completeResp.Body.Close()

	if completeResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(completeResp.Body)
		return fmt.Errorf("resumable upload complete failed (status %d): %s", completeResp.StatusCode, string(respBody))
	}

	return nil
}

// doNexusRequest makes an HTTP request to a Nexus-specific API endpoint.
func (c *Client) doNexusRequest(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	// Get the endpoint from the underlying S3 client options
	var endpoint string
	if c.client.Options().BaseEndpoint != nil {
		endpoint = *c.client.Options().BaseEndpoint
	}

	// Normalize endpoint and path
	endpoint = strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	url := endpoint + path

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Use the S3 client's HTTP client for consistent configuration
	httpClient := c.client.Options().HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return httpClient.Do(req)
}
