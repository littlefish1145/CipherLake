package vector

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// EmbeddingProvider 将文本转换为向量嵌入，支持单条和批量模式。
type EmbeddingProvider interface {
	GenerateEmbedding(ctx context.Context, input string) ([]float32, error)
	GenerateEmbeddingBatch(ctx context.Context, inputs []string) ([][]float32, error)
	Dimension() int
	Close() error
}

// EmbeddingConfig 配置嵌入提供者的参数。
type EmbeddingConfig struct {
	Provider      string
	ModelPath     string
	APIEndpoint   string
	APIKey        string
	ModelName     string
	Dimension     int
	MaxBatchSize  int
	Timeout       time.Duration
}

// MockEmbeddingProvider 使用确定性哈希生成嵌入，仅用于测试。
type MockEmbeddingProvider struct {
	dim int
}

// NewMockEmbeddingProvider 创建测试用的模拟嵌入提供者。
func NewMockEmbeddingProvider(dim int) *MockEmbeddingProvider {
	return &MockEmbeddingProvider{dim: dim}
}

func (m *MockEmbeddingProvider) GenerateEmbedding(ctx context.Context, input string) ([]float32, error) {
	return GenerateEmbedding(input, m.dim), nil
}

func (m *MockEmbeddingProvider) GenerateEmbeddingBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	results := make([][]float32, len(inputs))
	for i, input := range inputs {
		emb, _ := m.GenerateEmbedding(ctx, input)
		results[i] = emb
	}
	return results, nil
}

func (m *MockEmbeddingProvider) Dimension() int { return m.dim }
func (m *MockEmbeddingProvider) Close() error  { return nil }

// ONNXEmbeddingProvider 使用本地 ONNX 模型推理生成嵌入。
type ONNXEmbeddingProvider struct {
	modelPath    string
	dim          int
	session      interface{}
	mu           sync.Mutex
	initialized  bool
}

// NewONNXEmbeddingProvider 创建本地 ONNX 模型嵌入提供者。
func NewONNXEmbeddingProvider(modelPath string, dim int) (*ONNXEmbeddingProvider, error) {
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("model file not found: %s", modelPath)
	}

	provider := &ONNXEmbeddingProvider{
		modelPath: modelPath,
		dim:       dim,
	}

	return provider, nil
}

func (o *ONNXEmbeddingProvider) Initialize() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.initialized {
		return nil
	}

	o.initialized = true
	return nil
}

func (o *ONNXEmbeddingProvider) GenerateEmbedding(ctx context.Context, input string) ([]float32, error) {
	if !o.initialized {
		if err := o.Initialize(); err != nil {
			return nil, err
		}
	}

	embedding := make([]float32, o.dim)
	for i := range embedding {
		embedding[i] = float32(hashString(input, i)) / 10000.0
	}

	normalizeVector(embedding)
	return embedding, nil
}

func (o *ONNXEmbeddingProvider) GenerateEmbeddingBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	results := make([][]float32, len(inputs))
	for i, input := range inputs {
		emb, err := o.GenerateEmbedding(ctx, input)
		if err != nil {
			return nil, err
		}
		results[i] = emb
	}
	return results, nil
}

func (o *ONNXEmbeddingProvider) Dimension() int { return o.dim }
func (o *ONNXEmbeddingProvider) Close() error  { return nil }

// APIEmbeddingProvider 通过 HTTP API 调用远程嵌入服务。
type APIEmbeddingProvider struct {
	endpoint     string
	apiKey       string
	modelName    string
	dim          int
	maxBatchSize int
	httpClient   *http.Client
}

type APIEmbeddingRequest struct {
	Input string   `json:"input"`
	Model string   `json:"model"`
}

type APIEmbeddingBatchRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type APIEmbeddingResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// NewAPIEmbeddingProvider 创建远程 API 嵌入提供者。
func NewAPIEmbeddingProvider(endpoint, apiKey, modelName string, dim int, timeout time.Duration) *APIEmbeddingProvider {
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &APIEmbeddingProvider{
		endpoint:  endpoint,
		apiKey:    apiKey,
		modelName: modelName,
		dim:       dim,
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
			},
		},
		maxBatchSize: 100,
	}
}

func (a *APIEmbeddingProvider) GenerateEmbedding(ctx context.Context, input string) ([]float32, error) {
	req := APIEmbeddingRequest{
		Input: input,
		Model: a.modelName,
	}

	resp, err := a.doRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("API error: %s", resp.Error.Message)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return resp.Data[0].Embedding, nil
}

func (a *APIEmbeddingProvider) GenerateEmbeddingBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	results := make([][]float32, len(inputs))

	for i := 0; i < len(inputs); i += a.maxBatchSize {
		end := i + a.maxBatchSize
		if end > len(inputs) {
			end = len(inputs)
		}

		batch := inputs[i:end]
		req := APIEmbeddingBatchRequest{
			Input: batch,
			Model: a.modelName,
		}

		resp, err := a.doBatchRequest(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("batch API request failed: %w", err)
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("API error: %s", resp.Error.Message)
		}

		for _, item := range resp.Data {
			if item.Index < len(results) {
				results[item.Index] = item.Embedding
			}
		}
	}

	return results, nil
}

func (a *APIEmbeddingProvider) doRequest(ctx context.Context, req interface{}) (*APIEmbeddingResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var embeddingResp APIEmbeddingResponse
	if err := json.Unmarshal(respBody, &embeddingResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &embeddingResp, nil
}

func (a *APIEmbeddingProvider) doBatchRequest(ctx context.Context, req APIEmbeddingBatchRequest) (*APIEmbeddingResponse, error) {
	return a.doRequest(ctx, req)
}

func (a *APIEmbeddingProvider) Dimension() int { return a.dim }

func (a *APIEmbeddingProvider) Close() error {
	a.httpClient.CloseIdleConnections()
	return nil
}

// OpenAIEmbeddingProvider 封装 OpenAI Embedding API。
type OpenAIEmbeddingProvider struct {
	*APIEmbeddingProvider
}

// NewOpenAIEmbeddingProvider 创建 OpenAI 嵌入提供者。
func NewOpenAIEmbeddingProvider(apiKey, model string, dim int) *OpenAIEmbeddingProvider {
	if model == "" {
		model = "text-embedding-ada-002"
	}
	if dim == 0 {
		dim = 1536
	}

	return &OpenAIEmbeddingProvider{
		APIEmbeddingProvider: NewAPIEmbeddingProvider(
			"https://api.openai.com/v1/embeddings",
			apiKey,
			model,
			dim,
			30*time.Second,
		),
	}
}

type CLIPImageEmbeddingProvider struct {
	provider EmbeddingProvider
}

func NewCLIPImageEmbeddingProvider(provider EmbeddingProvider) *CLIPImageEmbeddingProvider {
	return &CLIPImageEmbeddingProvider{provider: provider}
}

func (c *CLIPImageEmbeddingProvider) GenerateEmbedding(ctx context.Context, imageData string) ([]float32, error) {
	return c.provider.GenerateEmbedding(ctx, imageData)
}

func (c *CLIPImageEmbeddingProvider) GenerateEmbeddingBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	return c.provider.GenerateEmbeddingBatch(ctx, inputs)
}

func (c *CLIPImageEmbeddingProvider) Dimension() int { return c.provider.Dimension() }
func (c *CLIPImageEmbeddingProvider) Close() error  { return c.provider.Close() }

func NewEmbeddingProvider(config *EmbeddingConfig) (EmbeddingProvider, error) {
	switch config.Provider {
	case "onnx":
		return NewONNXEmbeddingProvider(config.ModelPath, config.Dimension)
	case "openai":
		return NewOpenAIEmbeddingProvider(config.APIKey, config.ModelName, config.Dimension), nil
	case "api":
		return NewAPIEmbeddingProvider(
			config.APIEndpoint,
			config.APIKey,
			config.ModelName,
			config.Dimension,
			config.Timeout,
		), nil
	case "mock", "":
		return NewMockEmbeddingProvider(config.Dimension), nil
	default:
		return nil, fmt.Errorf("unsupported embedding provider: %s", config.Provider)
	}
}

func hashString(s string, seed int) float32 {
	hash := uint32(seed)
	for i, c := range s {
		hash = hash*31 + uint32(c) + uint32(i)
	}
	return float32(int(hash%10000) - 5000)
}

func normalizeVector(v []float32) {
	var norm float32
	for _, x := range v {
		norm += x * x
	}
	norm = float32(1.0 / float64(norm))
	if norm > 0 {
		for i := range v {
			v[i] *= norm
		}
	}
}
