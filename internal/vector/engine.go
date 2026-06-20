package vector

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrVectorNotFound      = errors.New("vector not found")
	ErrInvalidDimension    = errors.New("invalid vector dimension")
	ErrIndexNotInitialized = errors.New("index not initialized")
	ErrInvalidMetric       = errors.New("invalid metric type")
)

type MetricType string

const (
	MetricCosine     MetricType = "cosine"
	MetricEuclidean  MetricType = "euclidean"
	MetricDotProduct MetricType = "dot_product"
)

// VectorIndex 抽象向量索引后端,目前唯一实现是 MilvusBackend。
type VectorIndex interface {
	Insert(ctx context.Context, vectors []Vector) error
	Search(ctx context.Context, query Vector, topK int, filters map[string]string) ([]SearchResult, error)
	Delete(ctx context.Context, ids []string) error
	Build(ctx context.Context) error
	GetStats() IndexStats
}

type Vector struct {
	ID        string
	Values    []float32
	Metadata  map[string]string
	Bucket    string
	ObjectKey string
	Dimension int
	CreatedAt time.Time
	Checksum  string
}

type SearchResult struct {
	ID        string
	Score     float32
	Metadata  map[string]string
	Bucket    string
	ObjectKey string
}

type IndexStats struct {
	TotalVectors  int64
	HotVectors    int64
	ColdVectors   int64
	MemoryUsageMB float64
	IndexType     string
	Dimension     int
	LastBuiltAt   time.Time
	QueryCount    int64
	AvgLatencyMs  float64
}

// VectorManager 管理向量索引,统一使用 Milvus 作为后端。
// 自研 HNSW/IVFPQ/MMap/BucketIndex 已退役。
type VectorManager struct {
	mu                sync.RWMutex
	index             VectorIndex
	dim               int
	metric            MetricType
	config            *VectorConfig
	vectorMap         map[string]*Vector
	cache             *QueryCache
	embeddingCache    *embeddingCache
	embeddingProvider EmbeddingProvider
}

// embeddingCache 缓存 text -> embedding,避免重复 API 调用。
type embeddingCache struct {
	mu      sync.RWMutex
	cache   map[string]*cachedEmbedding
	maxSize int
	ttl     time.Duration
}

type cachedEmbedding struct {
	Vec       []float32
	CreatedAt time.Time
}

func newEmbeddingCache(maxSize int, ttl time.Duration) *embeddingCache {
	if maxSize <= 0 {
		maxSize = 256
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &embeddingCache{
		cache:   make(map[string]*cachedEmbedding),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

func (ec *embeddingCache) get(text string) ([]float32, bool) {
	ec.mu.RLock()
	defer ec.mu.RUnlock()
	if e, ok := ec.cache[text]; ok {
		if time.Since(e.CreatedAt) < ec.ttl {
			return e.Vec, true
		}
	}
	return nil, false
}

func (ec *embeddingCache) set(text string, vec []float32) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	if len(ec.cache) >= ec.maxSize {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range ec.cache {
			if oldestTime.IsZero() || v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
			}
		}
		delete(ec.cache, oldestKey)
	}
	ec.cache[text] = &cachedEmbedding{Vec: vec, CreatedAt: time.Now()}
}

// VectorConfig 是向量索引配置,全面转向 Milvus 后仅保留 Milvus 相关字段。
type VectorConfig struct {
	Enabled              bool
	Dimension            int
	IndexType            string
	MetricType           string
	MaxVectors           int64
	QueryCacheSize       int
	QueryCacheTTL        time.Duration
	EmbeddingProvider    string
	EmbeddingModelPath   string
	EmbeddingAPIEndpoint string
	EmbeddingAPIKey      string
	EmbeddingModelName   string
	// Security settings
	AutoIndex           bool
	MaxSearchTopK       int
	MaxQueryLength      int
	RequireAuth         bool
	AllowedContentTypes []string
	MaxIndexContentSize int64
	// Milvus 后端配置(必填,全面转向 Milvus)
	Milvus *MilvusConfig
}

type QueryCache struct {
	mu      sync.RWMutex
	cache   map[string]*cachedResult
	maxSize int
	ttl     time.Duration
}

type cachedResult struct {
	Results   []SearchResult
	CreatedAt time.Time
}

func NewQueryCache(maxSize int, ttl time.Duration) *QueryCache {
	return &QueryCache{
		cache:   make(map[string]*cachedResult),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

func (qc *QueryCache) get(key string) ([]SearchResult, bool) {
	qc.mu.RLock()
	defer qc.mu.RUnlock()

	if result, ok := qc.cache[key]; ok {
		if time.Since(result.CreatedAt) < qc.ttl {
			return result.Results, true
		}
	}

	return nil, false
}

func (qc *QueryCache) set(key string, results []SearchResult) {
	qc.mu.Lock()
	defer qc.mu.Unlock()

	if len(qc.cache) >= qc.maxSize {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range qc.cache {
			if oldestTime.IsZero() || v.CreatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CreatedAt
			}
		}
		delete(qc.cache, oldestKey)
	}

	qc.cache[key] = &cachedResult{
		Results:   results,
		CreatedAt: time.Now(),
	}
}

// NewVectorManager 创建向量管理器,统一使用 Milvus 后端。
// 如果 config.Milvus 为 nil,则不创建索引后端(用于测试或不启用向量索引的场景)。
func NewVectorManager(config *VectorConfig) (*VectorManager, error) {
	dim := config.Dimension
	if dim == 0 {
		dim = 768
	}

	metric := MetricType(config.MetricType)
	if metric == "" {
		metric = MetricCosine
	}

	var index VectorIndex
	var err error

	// 全面转向 Milvus:唯一支持的索引后端
	if config.Milvus != nil {
		index, err = NewMilvusBackend(dim, metric, config.Milvus)
		if err != nil {
			return nil, fmt.Errorf("failed to create milvus backend: %w", err)
		}
	}

	cacheSize := config.QueryCacheSize
	if cacheSize == 0 {
		cacheSize = 10000
	}

	cacheTTL := config.QueryCacheTTL
	if cacheTTL == 0 {
		cacheTTL = 5 * time.Minute
	}

	var embeddingProvider EmbeddingProvider
	if config.Enabled {
		embConfig := &EmbeddingConfig{
			Provider:      config.EmbeddingProvider,
			ModelPath:     config.EmbeddingModelPath,
			APIEndpoint:   config.EmbeddingAPIEndpoint,
			APIKey:        config.EmbeddingAPIKey,
			ModelName:     config.EmbeddingModelName,
			Dimension:     dim,
		}
		embeddingProvider, err = NewEmbeddingProvider(embConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create embedding provider: %w", err)
		}
	}

	return &VectorManager{
		index:             index,
		dim:               dim,
		metric:            metric,
		config:            config,
		vectorMap:         make(map[string]*Vector),
		cache:             NewQueryCache(cacheSize, cacheTTL),
		embeddingCache:    newEmbeddingCache(256, 10*time.Minute),
		embeddingProvider: embeddingProvider,
	}, nil
}

func (vm *VectorManager) IndexVector(ctx context.Context, v *Vector) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if v.ID == "" {
		v.ID = uuid.New().String()
	}
	if v.Dimension == 0 {
		v.Dimension = vm.dim
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}

	mapKey := v.Bucket + "/" + v.ObjectKey
	vm.vectorMap[mapKey] = v

	if vm.index == nil {
		return ErrIndexNotInitialized
	}
	return vm.index.Insert(ctx, []Vector{*v})
}

func (vm *VectorManager) IndexVectors(ctx context.Context, vectors []Vector) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	now := time.Now()
	for i := range vectors {
		v := &vectors[i]
		if v.ID == "" {
			v.ID = uuid.New().String()
		}
		if v.Dimension == 0 {
			v.Dimension = vm.dim
		}
		if v.CreatedAt.IsZero() {
			v.CreatedAt = now
		}
		mapKey := v.Bucket + "/" + v.ObjectKey
		vm.vectorMap[mapKey] = v
	}

	if vm.index == nil {
		return ErrIndexNotInitialized
	}
	if len(vectors) == 0 {
		return nil
	}
	return vm.index.Insert(ctx, vectors)
}

func (vm *VectorManager) Search(ctx context.Context, query Vector, topK int, filters map[string]string) ([]SearchResult, error) {
	cacheKey := vm.generateCacheKey(query, topK, filters)
	if results, ok := vm.cache.get(cacheKey); ok {
		return results, nil
	}

	vm.mu.RLock()
	idx := vm.index
	vm.mu.RUnlock()

	if idx == nil {
		return nil, ErrIndexNotInitialized
	}

	results, err := idx.Search(ctx, query, topK, filters)
	if err != nil {
		return nil, fmt.Errorf("index search failed: %w", err)
	}

	vm.cache.set(cacheKey, results)
	return results, nil
}

func (vm *VectorManager) generateCacheKey(query Vector, topK int, filters map[string]string) string {
	key := fmt.Sprintf("%v:%d", query.Values[:minInt(10, len(query.Values))], topK)
	for k, v := range filters {
		key += fmt.Sprintf(":%s=%s", k, v)
	}
	return key
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (vm *VectorManager) DeleteVector(ctx context.Context, bucket, objectKey string) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	mapKey := bucket + "/" + objectKey
	v, ok := vm.vectorMap[mapKey]
	if !ok {
		return ErrVectorNotFound
	}

	delete(vm.vectorMap, mapKey)

	if vm.index == nil {
		return ErrIndexNotInitialized
	}
	return vm.index.Delete(ctx, []string{v.ID})
}

func (vm *VectorManager) GetVector(ctx context.Context, bucket, objectKey string) (*Vector, error) {
	vm.mu.RLock()
	defer vm.mu.RUnlock()

	mapKey := bucket + "/" + objectKey
	v, ok := vm.vectorMap[mapKey]
	if !ok {
		return nil, ErrVectorNotFound
	}
	return v, nil
}

func (vm *VectorManager) RebuildIndex(ctx context.Context) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if vm.index == nil {
		return ErrIndexNotInitialized
	}
	return vm.index.Build(ctx)
}

func (vm *VectorManager) GetStats() map[string]IndexStats {
	vm.mu.RLock()
	defer vm.mu.RUnlock()

	if vm.index == nil {
		return map[string]IndexStats{}
	}
	return map[string]IndexStats{
		"index": vm.index.GetStats(),
	}
}

func (vm *VectorManager) GenerateEmbedding(ctx context.Context, input string) ([]float32, error) {
	if vm.embeddingProvider != nil {
		return vm.embeddingProvider.GenerateEmbedding(ctx, input)
	}
	return GenerateEmbedding(input, vm.dim), nil
}

func (vm *VectorManager) GenerateEmbeddingBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	if vm.embeddingProvider != nil {
		return vm.embeddingProvider.GenerateEmbeddingBatch(ctx, inputs)
	}

	results := make([][]float32, len(inputs))
	for i, input := range inputs {
		results[i] = GenerateEmbedding(input, vm.dim)
	}
	return results, nil
}

func (vm *VectorManager) IndexWithEmbedding(ctx context.Context, bucket, objectKey, text string, metadata map[string]string) error {
	embedding, err := vm.GenerateEmbedding(ctx, text)
	if err != nil {
		return fmt.Errorf("failed to generate embedding: %w", err)
	}

	vector := &Vector{
		ID:        uuid.New().String(),
		Values:    embedding,
		Metadata:  metadata,
		Bucket:    bucket,
		ObjectKey: objectKey,
		Dimension: vm.dim,
		CreatedAt: time.Now(),
	}

	return vm.IndexVector(ctx, vector)
}

func (vm *VectorManager) SearchByText(ctx context.Context, queryText string, topK int, filters map[string]string) ([]SearchResult, error) {
	var embedding []float32
	if vm.embeddingCache != nil {
		if cached, ok := vm.embeddingCache.get(queryText); ok {
			embedding = cached
		}
	}
	if embedding == nil {
		var err error
		embedding, err = vm.GenerateEmbedding(ctx, queryText)
		if err != nil {
			return nil, fmt.Errorf("failed to generate query embedding: %w", err)
		}
		if vm.embeddingCache != nil {
			vm.embeddingCache.set(queryText, embedding)
		}
	}

	query := Vector{
		Values:    embedding,
		Dimension: vm.dim,
	}

	return vm.Search(ctx, query, topK, filters)
}

func (vm *VectorManager) Close() error {
	if vm.index != nil {
		if mb, ok := vm.index.(*MilvusBackend); ok {
			mb.Close()
		}
	}
	if vm.embeddingProvider != nil {
		return vm.embeddingProvider.Close()
	}
	return nil
}

func EncodeVector(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}

	data := make([]byte, 4+len(v)*4)
	binary.LittleEndian.PutUint32(data[:4], uint32(len(v)))
	for i, f := range v {
		bits := math.Float32bits(f)
		binary.LittleEndian.PutUint32(data[4+i*4:4+i*4+4], bits)
	}

	return data
}

func DecodeVector(data []byte) []float32 {
	if len(data) < 4 {
		return nil
	}

	dim := binary.LittleEndian.Uint32(data[:4])
	if int(dim)*4+4 != len(data) {
		return nil
	}

	v := make([]float32, dim)
	for i := uint32(0); i < dim; i++ {
		bits := binary.LittleEndian.Uint32(data[4+i*4 : 4+i*4+4])
		v[i] = math.Float32frombits(bits)
	}

	return v
}

// IsTextContent checks if a content type is suitable for text-based embedding.
// Exported for use in gateway and tests.
func IsTextContent(contentType string) bool {
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
	textSuffixes := []string{"+json", "+xml", "+yaml"}
	for _, suffix := range textSuffixes {
		if strings.Contains(contentType, suffix) {
			return true
		}
	}
	return false
}

func GenerateEmbedding(text string, dim int) []float32 {
	v := make([]float32, dim)
	hash := uint32(0)
	for i, c := range text {
		hash = hash*31 + uint32(c) + uint32(i)
	}

	for i := 0; i < dim; i++ {
		v[i] = float32(hash%1000) / 1000.0
		hash = hash*1103515245 + 12345
	}

	return v
}
