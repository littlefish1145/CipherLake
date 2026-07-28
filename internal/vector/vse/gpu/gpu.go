// Package gpu provides GPU-accelerated vector search for cold segments.
//
// Architecture (configurable by batch size + dimension):
//
//   batch=1, dim≤256  ──►  CPU  (GPU overhead > benefit)
//   batch=1, dim>512  ──►  CPU coarse nProbe=64  →  GPU PQ decode + topK
//   batch≥16           ──►  Full GPU: cuBLAS coarse → GPU PQ decode → CPU topK
//   batch≥64 & cand≥5K ──►  + GPU exact rerank via cuBLAS Sgemv
//
// Data uploaded once per segment open (pinned on GPU):
//   - PQ codebook:  M × 256 × subdim float32
//   - PQ codes:     nv × M bytes
//   - Centroids:    nc × dim float32
//   - Exact vecs:   nv × dim float32  (only when rerank enabled)
package gpu

import (
	"cipherlake/internal/vector/vse"
	"cipherlake/internal/vector/vse/engine/quantizer"
	"errors"
)

var ErrNoGPU = errors.New("GPU not available; build with -tags cuda")

type Config struct {
	Enabled          bool   // master switch
	BatchThreshold   int    // min batch for full GPU pipeline
	DimThreshold     int    // min dim for GPU PQ decode path
	NProbe           int    // IVF nprobe for GPU path
	RerankBatch      int    // min batch for exact GPU rerank
	RerankCandidates int    // min candidates for exact GPU rerank
	PTXPath          string // path to pre-compiled kernels.ptx (empty = search default locations)
}

func DefaultConfig() Config {
	return Config{
		Enabled:          false,
		BatchThreshold:   16,
		DimThreshold:     512,
		NProbe:           64,
		RerankBatch:      64,
		RerankCandidates: 5000,
		PTXPath:          "",
	}
}

// ---------------------------------------------------------------------------
// Manager — the public API
// ---------------------------------------------------------------------------

type Manager struct {
	impl gpuImpl
}

type gpuImpl interface {
	init() error
	close()
	enabled() bool
	pinSegment(meta *vse.SegmentMeta, vectors []byte, pq *quantizer.PQQuantizer, ivf *quantizer.IVF) error
	unpinSegment(segID vse.SegmentID) error
	search(segID vse.SegmentID, req SearchRequest) ([]vse.SearchResult, error)
	batchSearch(segID vse.SegmentID, reqs []SearchRequest) []BatchResult
	pinVamanaGraph(segID vse.SegmentID, nbrOffsets, nbrData []int32) error
	hasVamanaGraph(segID vse.SegmentID) bool
	vamanaSearchGPU(segID vse.SegmentID, query []float32, topK, beamL int) ([]int32, []float32, error)
}

func NewManager(cfg Config) *Manager {
	return &Manager{impl: newGPUImpl(cfg)}
}

func (m *Manager) Init() error   { return m.impl.init() }
func (m *Manager) Close()        { m.impl.close() }
func (m *Manager) Enabled() bool { return m.impl.enabled() }

func (m *Manager) PinSegment(meta *vse.SegmentMeta, vectors []byte,
	pq *quantizer.PQQuantizer, ivf *quantizer.IVF) error {
	return m.impl.pinSegment(meta, vectors, pq, ivf)
}

func (m *Manager) UnpinSegment(segID vse.SegmentID) error {
	return m.impl.unpinSegment(segID)
}

// ---------------------------------------------------------------------------
// Search API
// ---------------------------------------------------------------------------

type SearchRequest struct {
	Query  []float32
	TopK   int
	Metric vse.MetricType
	NProbe int  // IVF nprobe (0 = use config default)
	Exact  bool // force exact (skip PQ)
}

type BatchResult struct {
	Results []vse.SearchResult
	Err     error
}

func (m *Manager) Search(segID vse.SegmentID, req SearchRequest) ([]vse.SearchResult, error) {
	return m.impl.search(segID, req)
}

func (m *Manager) BatchSearch(segID vse.SegmentID, reqs []SearchRequest) []BatchResult {
	return m.impl.batchSearch(segID, reqs)
}

func (m *Manager) PinVamanaGraph(segID vse.SegmentID, nbrOffsets, nbrData []int32) error {
	return m.impl.pinVamanaGraph(segID, nbrOffsets, nbrData)
}

func (m *Manager) HasVamanaGraph(segID vse.SegmentID) bool {
	return m.impl.hasVamanaGraph(segID)
}

func (m *Manager) VamanaSearchGPU(segID vse.SegmentID, query []float32, topK, beamL int) ([]int32, []float32, error) {
	return m.impl.vamanaSearchGPU(segID, query, topK, beamL)
}
