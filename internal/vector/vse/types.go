package vse

import (
	"math"
	"sync"
	"time"
)

type VectorID uint64
type SegmentID uint32

type QuantizerType int8

const (
	QuantizerNone QuantizerType = iota
	QuantizerSQ
	QuantizerPQ
)

type Tier int8

const (
	TierHot     Tier = 0
	TierCold    Tier = 1
	TierDiskANN Tier = 2 // DiskANN: Vamana graph + PQ stitched search, 磁盘友好
)

type MetricType int8

const (
	MetricCosine     MetricType = 0
	MetricEuclidean  MetricType = 1
	MetricDotProduct MetricType = 2
)

type SegmentMeta struct {
	ID            SegmentID
	Tier          Tier
	NumVectors    int
	Dimension     int
	QuantType     QuantizerType
	PQSubVecs     int
	PQBits        int
	CentroidCount int
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DataSize      int64
	VectorsFile   string
	GraphFile     string
	PQFile        string
	CentroidFile  string
	MetaFile      string

	// DiskANN-specific (TierDiskANN)
	VamanaR     int     // R parameter
	VamanaL     int     // L parameter
	VamanaAlpha float64 // alpha parameter
}

type SearchResult struct {
	ID        VectorID
	Score     float32
	SegmentID SegmentID
}

type VectorEntry struct {
	ID        VectorID
	ExtID     string
	Values    []float32
	Metadata  map[string]string
	Bucket    string
	ObjectKey string
	CreatedAt time.Time
}

type MergePolicy struct {
	HotTargetSize   int
	HotMaxSegments  int
	ColdTargetSize  int
	ColdMaxSegments int
	MinVectorsMerge int
	MergeInterval   time.Duration
	IdleThreshold   time.Duration
}

func DefaultMergePolicy() MergePolicy {
	return MergePolicy{
		HotTargetSize:   10000,
		HotMaxSegments:  10,
		ColdTargetSize:  100000,
		ColdMaxSegments: 50,
		MinVectorsMerge: 100,
		MergeInterval:   5 * time.Minute,
		IdleThreshold:   30 * time.Minute,
	}
}

type WarmupPolicy struct {
	Enabled         bool
	BatchSize       int
	BatchInterval   time.Duration
	PreloadCentroids bool
	PreloadPQ       bool
	MaxPreloadBytes  int64
}

func DefaultWarmupPolicy() WarmupPolicy {
	return WarmupPolicy{
		Enabled:          true,
		BatchSize:        5,
		BatchInterval:    100 * time.Millisecond,
		PreloadCentroids: true,
		PreloadPQ:        false,
		MaxPreloadBytes:  512 * 1024 * 1024,
	}
}

func L2DistanceSIMD(a, b []float32) float32 {
	var sum float32
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

func CosineDistanceSIMD(a, b []float32) float32 {
	var dot, normA, normB float32
	for i := range a {
		ai := a[i]
		bi := b[i]
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 1.0
	}
	return 1.0 - dot/float32(math.Sqrt(float64(normA*normB)))
}

func DotProductSIMD(a, b []float32) float32 {
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return 1.0 - dot
}

func NewMetricFunc(mt MetricType) func(a, b []float32) float32 {
	switch mt {
	case MetricEuclidean:
		return L2DistanceSIMD
	case MetricDotProduct:
		return DotProductSIMD
	default:
		return CosineDistanceSIMD
	}
}

type IDGenerator struct {
	mu sync.Mutex
	n  uint64
}

func (g *IDGenerator) Next() VectorID {
	g.mu.Lock()
	n := g.n
	g.n++
	g.mu.Unlock()
	return VectorID(n)
}

func (g *IDGenerator) Peek() VectorID {
	g.mu.Lock()
	n := g.n
	g.mu.Unlock()
	return VectorID(n)
}

func (g *IDGenerator) AdvanceTo(n uint64) {
	g.mu.Lock()
	if n > g.n {
		g.n = n
	}
	g.mu.Unlock()
}

type DiskANNConfig struct {
	Enabled      bool
	R            int     // Vamana max degree, default 32
	L            int     // Vamana search list size, default 64
	Alpha        float64 // Vamana pruning parameter, default 1.2
	PQSubVecs    int
	PQBits       int
	WarmEnabled  bool  // 启用 DiskANN warm tier（替代 IVF-PQ 冷段）
}

func DefaultDiskANNConfig() DiskANNConfig {
	return DiskANNConfig{
		R:          32,
		L:          64,
		Alpha:      1.2,
		PQSubVecs:  0,  // auto from dim / 4
		PQBits:     8,
		WarmEnabled: true,
	}
}

type VSEConfig struct {
	DataDir          string
	QuantizerType    string
	CacheSize        int
	EnableS3         bool
	S3Endpoint       string
	S3Region         string
	S3Bucket         string
	S3AccessKey      string
	S3SecretKey      string
	MergePolicy      MergePolicy
	WarmupPolicy     WarmupPolicy
	HNSWM            int
	HNSWEfSearch     int
	SearchWorkers    int
	GPUAccel         bool
	GPUBatchMin      int
	IVFCentroids     int
	IVFNProbe        int
	PQSubQuantizers  int
	PQBits           int
	DiskANN          DiskANNConfig
}
