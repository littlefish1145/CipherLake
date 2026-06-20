package pipeline

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nexus/internal/vector"
)

type vectorIndexPlugin struct {
	vm *vector.VectorManager
}

func (p *vectorIndexPlugin) Name() string { return "vector_index" }

func (p *vectorIndexPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	content, err := io.ReadAll(input.Content)
	if err != nil {
		return nil, err
	}
	text := string(content)
	metadata := input.UserMetadata
	if metadata == nil {
		metadata = make(map[string]string)
	}
	err = p.vm.IndexWithEmbedding(ctx, input.Bucket, input.Key, text, metadata)
	if err != nil {
		return nil, err
	}
	return &ProcessResult{
		UpdatedMetadata: map[string]string{
			"vector_indexed": "true",
		},
	}, nil
}

func (p *vectorIndexPlugin) CanStream() bool { return false }

func (p *vectorIndexPlugin) SupportedTypes() []string {
	return []string{"text/plain", "application/json", "application/pdf"}
}

func newTestVectorManager(t *testing.T) *vector.VectorManager {
	vm, err := vector.NewVectorManager(&vector.VectorConfig{
		Enabled:           true,
		Dimension:         8,
		IndexType:         "milvus",
		MetricType:        "cosine",
		EmbeddingProvider: "mock",
		Milvus: &vector.MilvusConfig{
			Address:        "localhost:19530",
			CollectionName: "nexus_test_vectors",
			IndexType:      "HNSW",
		},
	})
	if err != nil {
		t.Skipf("skipping test: Milvus not available: %v", err)
	}
	return vm
}

func TestStorageComputePipeline_ImageUpload(t *testing.T) {
	executor := NewPipelineExecutor(10)
	err := RegisterDefaultPlugins(executor)
	require.NoError(t, err)

	configData := []byte(`
pipelines:
  - name: image-upload-pipeline
    trigger: on_upload
    filter: "content-type matches image"
    steps:
      - name: compress-image
        plugin: image_compress
      - name: extract-metadata
        plugin: metadata_extract
    enabled: true
    priority: 1
`)
	err = executor.LoadConfigData(configData)
	require.NoError(t, err)

	input := &ObjectInput{
		Key:          "photos/sunset.jpg",
		Bucket:       "test-bucket",
		Content:      strings.NewReader("fake image data"),
		Size:         1024,
		ContentType:  "image/jpeg",
		UserMetadata: map[string]string{"author": "test-user"},
	}

	result, err := executor.Execute(context.Background(), "image-upload-pipeline", input)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "webp", result.UpdatedMetadata["compression"])
	assert.Equal(t, "80", result.UpdatedMetadata["quality"])
	assert.Equal(t, "image", result.UpdatedMetadata["extracted_from"])

	pipelines := executor.GetMatchingPipelines(context.Background(), TriggerOnUpload, "image/jpeg", nil)
	assert.NotEmpty(t, pipelines)

	pipelineNames := make(map[string]bool)
	for _, p := range pipelines {
		pipelineNames[p.Name] = true
	}
	assert.True(t, pipelineNames["image-upload-pipeline"])
}

func TestStorageComputePipeline_VectorIndexing(t *testing.T) {
	vm := newTestVectorManager(t)
	defer vm.Close()

	ctx := context.Background()

	err := vm.IndexWithEmbedding(ctx, "test-bucket", "docs/readme.txt", "This is a test document about storage systems", map[string]string{
		"content_type": "text/plain",
	})
	require.NoError(t, err)

	err = vm.IndexWithEmbedding(ctx, "test-bucket", "docs/guide.txt", "User guide for the Nexus storage platform", map[string]string{
		"content_type": "text/plain",
	})
	require.NoError(t, err)

	results, err := vm.SearchByText(ctx, "storage systems", 5, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, results)

	found := false
	for _, r := range results {
		if r.ObjectKey == "docs/readme.txt" && r.Bucket == "test-bucket" {
			found = true
			break
		}
	}
	assert.True(t, found)

	stats := vm.GetStats()
	assert.Equal(t, int64(2), stats["index"].TotalVectors)
}

func TestStorageComputePipeline_EndToEnd(t *testing.T) {
	executor := NewPipelineExecutor(10)
	err := RegisterDefaultPlugins(executor)
	require.NoError(t, err)

	vm := newTestVectorManager(t)
	defer vm.Close()

	vectorPlugin := &vectorIndexPlugin{vm: vm}
	err = executor.RegisterPlugin(vectorPlugin)
	require.NoError(t, err)

	configData := []byte(`
pipelines:
  - name: text-processing-pipeline
    trigger: on_upload
    steps:
      - name: extract-metadata
        plugin: metadata_extract
      - name: index-to-vector
        plugin: vector_index
    enabled: true
    priority: 1
`)
	err = executor.LoadConfigData(configData)
	require.NoError(t, err)

	ctx := context.Background()
	textContent := "Nexus is a distributed object storage system with compute capabilities"
	input := &ObjectInput{
		Key:          "docs/nexus-intro.txt",
		Bucket:       "test-bucket",
		Content:      strings.NewReader(textContent),
		Size:         int64(len(textContent)),
		ContentType:  "text/plain",
		UserMetadata: map[string]string{"category": "documentation"},
	}

	result, err := executor.Execute(ctx, "text-processing-pipeline", input)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "true", result.UpdatedMetadata["vector_indexed"])

	searchResults, err := vm.SearchByText(ctx, "distributed object storage", 5, map[string]string{
		"category": "documentation",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, searchResults)

	found := false
	for _, r := range searchResults {
		if r.ObjectKey == "docs/nexus-intro.txt" && r.Bucket == "test-bucket" {
			found = true
			assert.Equal(t, "documentation", r.Metadata["category"])
			break
		}
	}
	assert.True(t, found)
}

func TestStorageComputePipeline_PipelineFilter(t *testing.T) {
	executor := NewPipelineExecutor(10)
	err := RegisterDefaultPlugins(executor)
	require.NoError(t, err)

	configData := []byte(`
pipelines:
  - name: image-pipeline
    trigger: on_upload
    filter: "content-type matches image"
    steps:
      - name: compress
        plugin: image_compress
    enabled: true
    priority: 1
  - name: video-pipeline
    trigger: on_upload
    filter: "content-type matches video"
    steps:
      - name: thumbnail
        plugin: video_thumbnail
    enabled: true
    priority: 2
  - name: all-upload-pipeline
    trigger: on_upload
    steps:
      - name: extract-meta
        plugin: metadata_extract
    enabled: true
    priority: 3
  - name: disabled-pipeline
    trigger: on_upload
    steps:
      - name: extract-meta
        plugin: metadata_extract
    enabled: false
    priority: 4
`)
	err = executor.LoadConfigData(configData)
	require.NoError(t, err)

	ctx := context.Background()

	imagePipelines := executor.GetMatchingPipelines(ctx, TriggerOnUpload, "image/jpeg", nil)
	assert.Len(t, imagePipelines, 2)
	imageNames := make(map[string]bool)
	for _, p := range imagePipelines {
		imageNames[p.Name] = true
	}
	assert.True(t, imageNames["image-pipeline"])
	assert.True(t, imageNames["all-upload-pipeline"])

	videoPipelines := executor.GetMatchingPipelines(ctx, TriggerOnUpload, "video/mp4", nil)
	assert.Len(t, videoPipelines, 2)
	videoNames := make(map[string]bool)
	for _, p := range videoPipelines {
		videoNames[p.Name] = true
	}
	assert.True(t, videoNames["video-pipeline"])
	assert.True(t, videoNames["all-upload-pipeline"])

	textPipelines := executor.GetMatchingPipelines(ctx, TriggerOnUpload, "text/plain", nil)
	assert.Len(t, textPipelines, 1)
	assert.Equal(t, "all-upload-pipeline", textPipelines[0].Name)

	getPipelines := executor.GetMatchingPipelines(ctx, TriggerOnGet, "image/jpeg", nil)
	assert.Empty(t, getPipelines)

	pdfPipelines := executor.GetMatchingPipelines(ctx, TriggerOnUpload, "application/pdf", nil)
	assert.Len(t, pdfPipelines, 1)
	assert.Equal(t, "all-upload-pipeline", pdfPipelines[0].Name)
}

func TestStorageComputePipeline_MultipleSteps(t *testing.T) {
	executor := NewPipelineExecutor(10)
	err := RegisterDefaultPlugins(executor)
	require.NoError(t, err)

	configData := []byte(`
pipelines:
  - name: multi-step-image-pipeline
    trigger: on_upload
    filter: "content-type matches image"
    steps:
      - name: compress
        plugin: image_compress
      - name: resize
        plugin: image_resize
      - name: extract-metadata
        plugin: metadata_extract
    enabled: true
    priority: 1
`)
	err = executor.LoadConfigData(configData)
	require.NoError(t, err)

	input := &ObjectInput{
		Key:          "photos/landscape.png",
		Bucket:       "media-bucket",
		Content:      strings.NewReader("fake png data"),
		Size:         2048,
		ContentType:  "image/png",
		UserMetadata: map[string]string{},
	}

	result, err := executor.Execute(context.Background(), "multi-step-image-pipeline", input)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "webp", result.UpdatedMetadata["compression"])
	assert.Equal(t, "80", result.UpdatedMetadata["quality"])
	assert.Equal(t, "true", result.UpdatedMetadata["resized"])
	assert.Equal(t, "image", result.UpdatedMetadata["extracted_from"])

	executions := executor.ListExecutions("media-bucket", "photos/landscape.png")
	assert.NotEmpty(t, executions)
	assert.Equal(t, StatusCompleted, executions[0].Status)
	assert.Len(t, executions[0].Steps, 3)

	for _, step := range executions[0].Steps {
		assert.Equal(t, StatusCompleted, step.Status)
	}
}

func TestStorageComputePipeline_VectorSearchAfterIndex(t *testing.T) {
	vm := newTestVectorManager(t)
	defer vm.Close()

	ctx := context.Background()

	documents := []struct {
		bucket   string
		key      string
		text     string
		metadata map[string]string
	}{
		{"docs", "ml-intro.txt", "Introduction to machine learning algorithms and neural networks", map[string]string{"topic": "ml"}},
		{"docs", "dl-guide.txt", "Deep learning guide for neural network architectures", map[string]string{"topic": "ml"}},
		{"docs", "cooking.txt", "Best cooking recipes for Italian pasta and pizza", map[string]string{"topic": "cooking"}},
		{"docs", "storage.txt", "Distributed object storage system design patterns", map[string]string{"topic": "storage"}},
		{"docs", "go-programming.txt", "Go programming language concurrency patterns", map[string]string{"topic": "programming"}},
	}

	for _, doc := range documents {
		err := vm.IndexWithEmbedding(ctx, doc.bucket, doc.key, doc.text, doc.metadata)
		require.NoError(t, err)
	}

	results, err := vm.SearchByText(ctx, "machine learning algorithms", 3, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, results)
	assert.LessOrEqual(t, len(results), 3)

	mlResults, err := vm.SearchByText(ctx, "neural network", 5, map[string]string{"topic": "ml"})
	require.NoError(t, err)
	assert.NotEmpty(t, mlResults)
	for _, r := range mlResults {
		assert.Equal(t, "ml", r.Metadata["topic"])
	}

	cookingResults, err := vm.SearchByText(ctx, "Italian pasta recipe", 3, map[string]string{"topic": "cooking"})
	require.NoError(t, err)
	if len(cookingResults) > 0 {
		assert.Equal(t, "cooking", cookingResults[0].Metadata["topic"])
		assert.Equal(t, "docs", cookingResults[0].Bucket)
		assert.Equal(t, "cooking.txt", cookingResults[0].ObjectKey)
	}

	stats := vm.GetStats()
	assert.Equal(t, int64(5), stats["index"].TotalVectors)

	queryVec := vector.GenerateEmbedding("machine learning algorithms", 8)
	directResults, err := vm.Search(ctx, vector.Vector{Values: queryVec, Dimension: 8}, 2, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, directResults)
}
