package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nexus/internal/auth"
	"nexus/internal/common"
	"nexus/internal/fts"
	"nexus/internal/pipeline"
	"nexus/internal/taskqueue"
	"nexus/internal/vector"

	"go.uber.org/zap"
)

type SearchService struct {
	gateway *S3Gateway
}

func NewSearchService(gateway *S3Gateway) *SearchService {
	return &SearchService{gateway: gateway}
}

type VectorSearchResponse struct {
	Results   []SearchResultItem `json:"results"`
	LatencyMs int64              `json:"latency_ms"`
	IndexUsed string             `json:"index_used"`
}

type SearchResultItem struct {
	Key      string            `json:"key"`
	Score    float32           `json:"score"`
	Metadata map[string]string `json:"metadata"`
}

type FTSSearchResponse struct {
	Results   []FTSSearchResultItem `json:"results"`
	Total     int                   `json:"total"`
	LatencyMs int64                 `json:"latency_ms"`
}

type FTSSearchResultItem struct {
	ID        string   `json:"id"`
	Bucket    string   `json:"bucket"`
	Key       string   `json:"key"`
	Score     float64  `json:"score"`
	Snippet   string   `json:"snippet"`
	Highlight []string `json:"highlight"`
}

func (s *SearchService) handleVectorSearch(w http.ResponseWriter, r *http.Request) error {
	identity, err := s.gateway.getIdentity(r)
	if err != nil {
		return fmt.Errorf("vector search requires authentication: %w", err)
	}
	if identity == nil {
		return fmt.Errorf("vector search requires authentication")
	}

	if identity.Role != "admin" {
		if s.gateway.permChecker != nil {
			if err := s.gateway.permChecker.Check(r.Context(), identity, "vector:Search", "", "", r); err != nil {
				hasVectorPerm := false
				for _, p := range identity.Permissions {
					if p == "vector:Search" || p == "vector:*" || p == "read" || p == "admin" {
						hasVectorPerm = true
						break
					}
				}
				if !hasVectorPerm {
					return fmt.Errorf("access denied: vector:Search permission required")
				}
			}
		} else {
			hasVectorPerm := false
			for _, p := range identity.Permissions {
				if p == "vector:Search" || p == "vector:*" || p == "read" || p == "admin" {
					hasVectorPerm = true
					break
				}
			}
			if !hasVectorPerm {
				return fmt.Errorf("access denied: vector:Search permission required")
			}
		}
	}

	if s.gateway.rateLimiter != nil {
		clientIP := extractClientIP(r)
		result := s.gateway.rateLimiter.Allow(r.Context(), clientIP, identity.ID, "", "VECTOR_SEARCH", 0)
		if !result.Allowed {
			return fmt.Errorf("vector search rate limit exceeded for user %s", identity.ID)
		}
	}

	query := r.URL.Query()
	searchQuery := query.Get("vector_search")
	if searchQuery == "" {
		return fmt.Errorf("missing vector_search query parameter")
	}

	if len(searchQuery) > 10000 {
		return fmt.Errorf("search query too long: maximum 10000 characters")
	}

	topKStr := query.Get("top_k")
	thresholdStr := query.Get("threshold")

	topK := 20
	if topKStr != "" {
		if k, err := strconv.Atoi(topKStr); err == nil {
			topK = k
		}
	}

	if topK > 100 {
		topK = 100
	}
	if topK <= 0 {
		topK = 1
	}

	threshold := float32(0.7)
	if thresholdStr != "" {
		if t, err := strconv.ParseFloat(thresholdStr, 32); err == nil {
			threshold = float32(t)
		}
	}

	filters := make(map[string]string)
	filterStr := query.Get("filter")
	if filterStr != "" {
		pairs := strings.Split(filterStr, "&")
		for _, pair := range pairs {
			kv := strings.Split(pair, "=")
			if len(kv) == 2 {
				filters[kv[0]] = kv[1]
			}
		}
	}

	if identity.Role != "admin" && s.gateway.permChecker != nil {
		if bucketFilter := filters["bucket"]; bucketFilter != "" {
			if err := s.gateway.permChecker.Check(r.Context(), identity, auth.ActionRead, bucketFilter, "", r); err != nil {
				return fmt.Errorf("access denied: no read permission on bucket %s", bucketFilter)
			}
		}
	}

	startTime := time.Now()

	searchResult, err := s.gateway.vector.SearchByText(r.Context(), searchQuery, topK, filters)
	if err != nil {
		return fmt.Errorf("vector search failed: %w", err)
	}

	results := make([]SearchResultItem, 0)
	for _, sr := range searchResult {
		if sr.Score < threshold {
			continue
		}
		if identity.Role != "admin" && s.gateway.permChecker != nil {
			if err := s.gateway.permChecker.Check(r.Context(), identity, auth.ActionRead, sr.Bucket, sr.ObjectKey, r); err != nil {
				continue
			}
		}
		results = append(results, SearchResultItem{
			Key:      sr.ObjectKey,
			Score:    sr.Score,
			Metadata: sr.Metadata,
		})
	}

	response := VectorSearchResponse{
		Results:   results,
		LatencyMs: time.Since(startTime).Milliseconds(),
		IndexUsed: "hot",
	}

	userID := "anonymous"
	if identity != nil {
		userID = identity.ID
	}
	s.gateway.auditLog(r, "VECTOR_SEARCH", "", "", userID, "success", map[string]interface{}{
		"query":      truncateString(searchQuery, 200),
		"top_k":      topK,
		"results":    len(results),
		"latency_ms": response.LatencyMs,
	})

	return s.gateway.writeJSON(w, http.StatusOK, response)
}

func (s *SearchService) handleVectorSearchForObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	if _, err := s.gateway.requireIdentity(r, auth.ActionRead, bucket, key); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}
	return s.handleVectorSearch(w, r)
}

func (s *SearchService) handleFTSSearch(w http.ResponseWriter, r *http.Request) error {
	if s.gateway.ftsIndex == nil {
		return fmt.Errorf("FTS is not enabled")
	}

	if _, err := s.gateway.requireIdentity(r, auth.ActionRead, "", ""); err != nil {
		return fmt.Errorf("FTS search requires authentication: %w", err)
	}

	query := r.URL.Query()
	searchQuery := query.Get("q")
	if searchQuery == "" {
		searchQuery = query.Get("fts_search")
	}
	if searchQuery == "" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
		if err == nil && len(body) > 0 {
			var req struct {
				Query  string `json:"query"`
				Bucket string `json:"bucket"`
				TopK   int    `json:"top_k"`
			}
			if json.Unmarshal(body, &req) == nil && req.Query != "" {
				searchQuery = req.Query
			}
		}
	}

	if searchQuery == "" {
		return fmt.Errorf("missing search query parameter 'q'")
	}

	if len(searchQuery) > 10000 {
		return fmt.Errorf("search query too long: maximum 10000 characters")
	}

	bucket := query.Get("bucket")
	topKStr := query.Get("topK")
	if topKStr == "" {
		topKStr = query.Get("top_k")
	}

	topK := 20
	if topKStr != "" {
		if k, err := strconv.Atoi(topKStr); err == nil {
			topK = k
		}
	}
	if topK > 100 {
		topK = 100
	}
	if topK <= 0 {
		topK = 1
	}

	startTime := time.Now()

	results, err := s.gateway.ftsIndex.Search(searchQuery, topK)
	if err != nil {
		return fmt.Errorf("FTS search failed: %w", err)
	}

	if bucket != "" {
		var filtered []fts.SearchResult
		for _, r := range results {
			if r.Bucket == bucket {
				filtered = append(filtered, r)
			}
		}
		results = filtered
	}

	queryTokens := fts.Tokenize(searchQuery)
	responseResults := make([]FTSSearchResultItem, 0, len(results))

	for _, r := range results {
		item := FTSSearchResultItem{
			ID:     fmt.Sprintf("%d", r.DocID),
			Bucket: r.Bucket,
			Key:    r.Key,
			Score:  r.Score,
		}

		content := s.getObjectTextContent(r.Bucket, r.Key)
		if content != "" {
			item.Snippet = fts.GenerateSnippet(content, queryTokens, 100)
			highlighted := fts.HighlightTerms(content, queryTokens)
			highlights := extractHighlights(highlighted)
			item.Highlight = highlights
		}

		responseResults = append(responseResults, item)
	}

	response := FTSSearchResponse{
		Results:   responseResults,
		Total:     len(responseResults),
		LatencyMs: time.Since(startTime).Milliseconds(),
	}

	return s.gateway.writeJSON(w, http.StatusOK, response)
}

func (s *SearchService) handleAdminFTSSearch(w http.ResponseWriter, r *http.Request) {
	if s.gateway.ftsIndex == nil {
		s.gateway.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "FTS is not enabled"})
		return
	}

	if err := s.handleFTSSearch(w, r); err != nil {
		s.gateway.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func (s *SearchService) getObjectTextContent(bucket, key string) string {
	objMeta, err := s.gateway.metadata.GetObject(context.Background(), bucket, key)
	if err != nil {
		return ""
	}

	contentType := objMeta.ContentType
	if !isTextContentType(contentType) {
		return ""
	}

	storageTier := common.StorageTier(objMeta.StorageTier)
	reader, _, err := s.gateway.store.Get(context.Background(), bucket, key, storageTier)
	if err != nil {
		return ""
	}
	defer reader.Close()

	data, err := io.ReadAll(io.LimitReader(reader, 1024*1024))
	if err != nil {
		return ""
	}

	return string(data)
}

func isTextContentType(contentType string) bool {
	textPrefixes := []string{
		"text/",
		"application/json",
		"application/xml",
		"application/javascript",
		"application/x-yaml",
		"application/markdown",
	}
	for _, prefix := range textPrefixes {
		if strings.HasPrefix(contentType, prefix) {
			return true
		}
	}
	return false
}

func extractHighlights(highlighted string) []string {
	var highlights []string
	for {
		startIdx := strings.Index(highlighted, "<em>")
		if startIdx == -1 {
			break
		}
		endIdx := strings.Index(highlighted, "</em>")
		if endIdx == -1 {
			break
		}
		term := highlighted[startIdx+4 : endIdx]
		highlights = append(highlights, "<em>"+term+"</em>")
		highlighted = highlighted[endIdx+5:]
	}
	return highlights
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (s *SearchService) vectorizeObject(ctx context.Context, bucket, key, contentType string, metadataMap map[string]string, userID string) {
	if !vector.IsTextContent(contentType) {
		return
	}

	objMeta, err := s.gateway.metadata.GetObject(ctx, bucket, key)
	if err != nil {
		zap.L().Warn("vectorize: GetObject failed", zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
		return
	}

	storageTier := common.StorageTier(objMeta.StorageTier)
	reader, _, err := s.gateway.store.Get(ctx, bucket, key, storageTier)
	if err != nil {
		zap.L().Warn("vectorize: store.Get failed", zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
		return
	}
	defer reader.Close()

	var dataReader io.Reader = reader
	if objMeta.Encrypted && s.gateway.cryptoCoordinator != nil && len(objMeta.EncryptedDEK) > 0 {
		decryptedReader, err := s.gateway.cryptoCoordinator.DecryptOperation(ctx, userID, bucket, key, reader, "", objMeta.EncryptedDEK)
		if err != nil {
			zap.L().Warn("vectorize: DecryptOperation failed", zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
			return
		}
		dataReader = decryptedReader
	}

	limitedReader := io.LimitReader(dataReader, 1024*1024)
	content, err := io.ReadAll(limitedReader)
	if err != nil {
		zap.L().Warn("vectorize: ReadAll failed", zap.Error(err))
		return
	}

	text := string(content)
	if len(strings.TrimSpace(text)) == 0 {
		zap.L().Warn("vectorize: empty content", zap.String("bucket", bucket), zap.String("key", key))
		return
	}

	vecMetadata := make(map[string]string)
	for k, v := range metadataMap {
		vecMetadata[k] = v
	}
	vecMetadata["content_type"] = contentType
	vecMetadata["indexed_by"] = userID
	vecMetadata["indexed_at"] = time.Now().Format(time.RFC3339)

	err = s.gateway.vector.IndexWithEmbedding(ctx, bucket, key, text, vecMetadata)
	if err != nil {
		zap.L().Warn("vectorize: IndexWithEmbedding failed", zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
		return
	}

	s.gateway.auditLog(nil, "VECTOR_INDEX", bucket, key, userID, "success", map[string]interface{}{
		"content_type": contentType,
		"content_size": len(content),
	})
}

func (s *SearchService) ftsIndexObject(ctx context.Context, bucket, key, contentType, versionID string) {
	if !isTextContentType(contentType) {
		return
	}

	objMeta, err := s.gateway.metadata.GetObject(ctx, bucket, key)
	if err != nil {
		return
	}

	storageTier := common.StorageTier(objMeta.StorageTier)
	reader, _, err := s.gateway.store.Get(ctx, bucket, key, storageTier)
	if err != nil {
		return
	}
	defer reader.Close()

	var dataReader io.Reader = reader
	if objMeta.Encrypted && s.gateway.cryptoCoordinator != nil && len(objMeta.EncryptedDEK) > 0 {
		decryptedReader, err := s.gateway.cryptoCoordinator.DecryptOperation(ctx, "", bucket, key, reader, "", objMeta.EncryptedDEK)
		if err != nil {
			return
		}
		dataReader = decryptedReader
	}

	limitedReader := io.LimitReader(dataReader, 1024*1024)
	content, err := io.ReadAll(limitedReader)
	if err != nil {
		return
	}

	text := string(content)
	if len(strings.TrimSpace(text)) == 0 {
		return
	}

	if err := s.gateway.ftsIndex.AddDocumentWithInfo(bucket, key, versionID, text); err != nil {
		return
	}
}

// handleVectorizeTask is the queued handler for vector indexing. It decodes
// the task payload and delegates to vectorizeObject, logging errors instead of
// silently dropping them.
func (s *SearchService) handleVectorizeTask(ctx context.Context, task *taskqueue.Task) error {
	var payload taskqueue.VectorizePayload
	if err := task.ParsePayload(&payload); err != nil {
		return fmt.Errorf("failed to parse vectorize payload: %w", err)
	}
	s.vectorizeObject(ctx, payload.Bucket, payload.Key, payload.ContentType, payload.Metadata, payload.UserID)
	return nil
}

// handleFTSTask is the queued handler for full-text indexing.
func (s *SearchService) handleFTSTask(ctx context.Context, task *taskqueue.Task) error {
	var payload taskqueue.FTSPayload
	if err := task.ParsePayload(&payload); err != nil {
		return fmt.Errorf("failed to parse fts payload: %w", err)
	}
	s.ftsIndexObject(ctx, payload.Bucket, payload.Key, payload.ContentType, payload.VersionID)
	return nil
}

// handlePipelineTask is the queued handler for pipeline execution.
func (g *S3Gateway) handlePipelineTask(ctx context.Context, task *taskqueue.Task) error {
	var payload taskqueue.PipelinePayload
	if err := task.ParsePayload(&payload); err != nil {
		return fmt.Errorf("failed to parse pipeline payload: %w", err)
	}

	if g.pipeline == nil {
		return nil
	}

	pipelines := g.pipeline.GetMatchingPipelines(ctx, pipeline.TriggerOnUpload, payload.ContentType, payload.Metadata)
	if len(pipelines) == 0 {
		return nil
	}

	objMeta, err := g.metadata.GetObject(ctx, payload.Bucket, payload.Key)
	if err != nil {
		return fmt.Errorf("failed to get object metadata: %w", err)
	}

	storageTier := common.StorageTier(objMeta.StorageTier)
	reader, _, err := g.store.Get(ctx, payload.Bucket, payload.Key, storageTier)
	if err != nil {
		return fmt.Errorf("failed to get object content: %w", err)
	}
	defer reader.Close()

	for _, p := range pipelines {
		// Reset reader for each pipeline
		input := &pipeline.ObjectInput{
			Key:          payload.Key,
			Bucket:       payload.Bucket,
			Content:      reader,
			Size:         objMeta.Size,
			ContentType:  payload.ContentType,
			UserMetadata: payload.Metadata,
		}
		if _, err := g.pipeline.Execute(ctx, p.Name, input); err != nil {
			zap.L().Error("pipeline execution failed",
				zap.String("pipeline", p.Name),
				zap.String("bucket", payload.Bucket),
				zap.String("key", payload.Key),
				zap.Error(err))
			return err
		}
	}
	return nil
}
