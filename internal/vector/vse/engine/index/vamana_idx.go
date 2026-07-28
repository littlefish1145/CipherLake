package index

import (
	"fmt"

	"cipherlake/internal/vector/vse/engine/quantizer"
	"cipherlake/internal/vector/vse/simd"
)

// VamanaIndex wraps VamanaGraph with the same interface as HNSWIndex.
type VamanaIndex struct {
	graph *VamanaGraph
	dim   int
	nodes int
}

func NewVamanaIndex(dim, R, L int, alpha float64, df func(a, b []float32) float32) *VamanaIndex {
	mt := detectMetric(df)
	g := NewVamanaGraph(dim, R, L, alpha, mt)
	return &VamanaIndex{graph: g, dim: dim}
}

func WrapVamanaGraph(g *VamanaGraph) *VamanaIndex {
	return &VamanaIndex{
		graph: g,
		dim:   g.Dim(),
		nodes: g.Len(),
	}
}

// Graph 返回内部 VamanaGraph 引用（用于邻居数据访问等）
func (idx *VamanaIndex) Graph() *VamanaGraph { return idx.graph }

func (idx *VamanaIndex) Insert(id uint64, vec []float32) error {
	if len(vec) != idx.dim {
		return fmt.Errorf("dimension mismatch: expected %d, got %d", idx.dim, len(vec))
	}
	if err := idx.graph.Insert(id, vec); err != nil {
		return err
	}
	idx.nodes++
	return nil
}

func (idx *VamanaIndex) InsertBatch(ids []uint64, vecs [][]float32) error {
	if len(ids) != len(vecs) {
		return fmt.Errorf("ids and vecs length mismatch")
	}
	for i := range ids {
		if len(vecs[i]) != idx.dim {
			return fmt.Errorf("dimension mismatch at index %d", i)
		}
		if err := idx.graph.Insert(ids[i], vecs[i]); err != nil {
			return err
		}
	}
	idx.nodes += len(ids)
	return nil
}

func (idx *VamanaIndex) Search(query []float32, topK int) ([]HNSWSearchResult, error) {
	if len(query) != idx.dim {
		return nil, fmt.Errorf("dimension mismatch: expected %d, got %d", idx.dim, len(query))
	}
	return idx.graph.Search(query, topK)
}

func (idx *VamanaIndex) StitchedSearch(
	query []float32,
	topK int,
	pqCodes [][]byte,
	pqTable [][]float32,
	distanceADC func(codes []byte) float32,
) ([]HNSWSearchResult, error) {
	if len(query) != idx.dim {
		return nil, fmt.Errorf("dimension mismatch: expected %d, got %d", idx.dim, len(query))
	}
	return idx.graph.StitchedSearch(query, topK, pqCodes, pqTable, distanceADC)
}

func (idx *VamanaIndex) Close() error {
	return idx.graph.Close()
}

func (idx *VamanaIndex) Delete(id uint64) bool {
	return false
}

func (idx *VamanaIndex) Len() int {
	return idx.graph.Len()
}

func (idx *VamanaIndex) WriteFile(path string) error {
	return idx.graph.WriteFile(path)
}

func (idx *VamanaIndex) Serialize() ([]byte, error) {
	return idx.graph.Serialize()
}

func (idx *VamanaIndex) Deserialize(data []byte, dim int, df func(a, b []float32) float32) error {
	graph := NewVamanaGraph(dim, idx.graph.R, idx.graph.L, idx.graph.alpha, detectMetric(df))
	if err := graph.Deserialize(data); err != nil {
		return fmt.Errorf("vamana deserialize: %w", err)
	}
	idx.graph = graph
	idx.dim = dim
	idx.nodes = graph.Len()
	return nil
}

// ---------------------------------------------------------------------------
// DiskANNIndex — Vamana graph + PQ codes for stitched search
//
// Stores exact vectors and PQ codes side by side. Search does two passes:
//   1. Graph traversal using PQ-ADC distances (fast, 2-4 bytes/dim)
//   2. Exact SIMD reranking of top candidates
// ---------------------------------------------------------------------------

type DiskANNIndex struct {
	Graph   *VamanaIndex
	PQ      *quantizer.PQQuantizer
	PQCodes [][]byte // PQ code per node, same order as graph nodes
	SimdFn  func(a, b []float32) float32
	Dim     int
	Nodes   int
}

func NewDiskANNIndex(dim, R, L int, alpha float64, pq *quantizer.PQQuantizer, df func(a, b []float32) float32) *DiskANNIndex {
	simd.Init()
	return &DiskANNIndex{
		Graph:  NewVamanaIndex(dim, R, L, alpha, df),
		PQ:     pq,
		SimdFn: df,
		Dim:    dim,
	}
}

// Build constructs the Vamana graph and encodes all vectors with PQ.
func (d *DiskANNIndex) Build(ids []uint64, vectors [][]float32) error {
	// Step 1: PQ training
	if d.PQ != nil && !d.PQ.Trained() {
		if err := d.PQ.Train(vectors); err != nil {
			return fmt.Errorf("pq train: %w", err)
		}
	}

	// Step 2: encode all vectors
	if d.PQ != nil {
		d.PQCodes = make([][]byte, len(vectors))
		for i, v := range vectors {
			code, err := d.PQ.Encode(v)
			if err != nil {
				return fmt.Errorf("pq encode %d: %w", i, err)
			}
			d.PQCodes[i] = code
		}
	}

	// Step 3: build Vamana graph
	flatVecs := make([]float32, len(vectors)*d.Dim)
	for i, v := range vectors {
		copy(flatVecs[i*d.Dim:(i+1)*d.Dim], v)
	}
	if err := d.Graph.graph.Build(ids, flatVecs); err != nil {
		return fmt.Errorf("vamana build: %w", err)
	}
	return nil
}

// BuildFromFlat builds the Vamana graph from flat float32 arrays.
func (d *DiskANNIndex) BuildFromFlat(ids []uint64, flatVecs []float32) error {
	n := len(ids)
	if len(flatVecs) < n*d.Dim {
		return fmt.Errorf("flatVecs too short: need %d, got %d", n*d.Dim, len(flatVecs))
	}

	// PQ training from flat
	if d.PQ != nil && !d.PQ.Trained() {
		if err := d.PQ.TrainFromFlat(flatVecs, n); err != nil {
			return fmt.Errorf("pq train from flat: %w", err)
		}
	}

	// PQ encode
	if d.PQ != nil {
		d.PQCodes = make([][]byte, n)
		for i := 0; i < n; i++ {
			code, err := d.PQ.Encode(flatVecs[i*d.Dim : (i+1)*d.Dim])
			if err != nil {
				return fmt.Errorf("pq encode %d: %w", i, err)
			}
			d.PQCodes[i] = code
		}
	}

	// Vamana build
	if err := d.Graph.graph.Build(ids, flatVecs); err != nil {
		return fmt.Errorf("vamana build: %w", err)
	}
	d.Nodes = n
	return nil
}

// BuildStreaming 支持分批构建 Vamana 图 + PQ 码表。
// batchProvider 返回每批的 (ids, flatVecs, error)，当返回 (nil, nil, nil) 时视为完成。
func (d *DiskANNIndex) BuildStreaming(totalCount int, batchProvider func(batchIdx int) (ids []uint64, flatVecs []float32, err error)) error {
	if totalCount <= 0 {
		return fmt.Errorf("totalCount must be > 0")
	}

	// Phase 1: 收集采样（最多 65536 向量）用于 PQ 训练
	sampleSize := totalCount
	if sampleSize > 65536 {
		sampleSize = 65536
	}
	sampleIDs := make([]uint64, 0, sampleSize)
	sampleVecs := make([]float32, 0, sampleSize*d.Dim)
	sampled := 0

	batchIdx := 0
	for sampled < totalCount {
		ids, vecs, err := batchProvider(batchIdx)
		if err != nil {
			return fmt.Errorf("batch %d: %w", batchIdx, err)
		}
		if ids == nil || len(ids) == 0 {
			break
		}
		n := len(ids)
		need := sampleSize - len(sampleIDs)
		if need > n {
			need = n
		}
		sampleIDs = append(sampleIDs, ids[:need]...)
		sampleVecs = append(sampleVecs, vecs[:need*d.Dim]...)
		sampled += n
		batchIdx++
	}

	// PQ training
	if d.PQ != nil && !d.PQ.Trained() && len(sampleVecs) > 0 {
		if err := d.PQ.TrainFromFlat(sampleVecs, len(sampleIDs)); err != nil {
			return fmt.Errorf("pq train streaming: %w", err)
		}
	}

	// Phase 2: 逐批构建
	d.PQCodes = make([][]byte, totalCount)
	codeIdx := 0

	batchIdx = 0
	sampled = 0
	for sampled < totalCount {
		ids, vecs, err := batchProvider(batchIdx)
		if err != nil {
			return err
		}
		if ids == nil || len(ids) == 0 {
			break
		}
		n := len(ids)

		// PQ encode
		if d.PQ != nil {
			for i := 0; i < n; i++ {
				code, err := d.PQ.Encode(vecs[i*d.Dim : (i+1)*d.Dim])
				if err != nil {
					return fmt.Errorf("pq encode streaming %d: %w", codeIdx+i, err)
				}
				d.PQCodes[codeIdx+i] = code
			}
		}

		// Vamana build (incremental, each batch uses Insert or Build)
		if err := d.Graph.graph.Build(ids, vecs); err != nil {
			return fmt.Errorf("vamana build batch %d: %w", batchIdx, err)
		}

		codeIdx += n
		sampled += n
		batchIdx++
	}

	d.Nodes = totalCount
	return nil
}

// PrefetchGraph 对一组候选节点的邻居执行异步预取（madvise WILLNEED）。
// 在 mmap 场景下，这能触发操作系统将邻居数据页调入 page cache。
func (d *DiskANNIndex) PrefetchGraph(candidates []int32) {
	g := d.Graph.graph
	for _, nid := range candidates {
		if int(nid) >= g.count {
			continue
		}
		nbrs := g.neighbors[nid]
		if len(nbrs) == 0 {
			continue
		}
		// 计算邻居数据所在页的偏移，madvise 预读
		// 简单实现：touch 邻居数据的第一个和最后一个字节
		_ = nbrs[0]
		_ = nbrs[len(nbrs)-1]

		// 预取每个邻居的向量（第一个和最后一个 float32）
		for _, nb := range nbrs {
			if int(nb) < g.count {
				_ = g.nodeVector(nb)[0]
				_ = g.nodeVector(nb)[g.dim-1]
			}
		}
	}
}

// SearchDiskANN performs the full DiskANN stitched search.
// Uses PQ-ADC for graph traversal, then SIMD exact for reranking.
func (d *DiskANNIndex) SearchDiskANN(query []float32, topK int) ([]HNSWSearchResult, error) {
	if d.PQ == nil || len(d.PQCodes) == 0 {
		return d.Graph.Search(query, topK)
	}

	// Precompute PQ distance table for this query
	table := d.PQ.PrecomputeQueryDistances(query)

	// Closure for ADC distance using the precomputed table
	adcFn := func(codes []byte) float32 {
		return d.PQ.DistanceADC(table, codes)
	}

	return d.Graph.graph.StitchedSearch(query, topK, d.PQCodes, table, adcFn)
}

// SearchExact delegates to pure SIMD Vamana search (no PQ).
func (d *DiskANNIndex) SearchExact(query []float32, topK int) ([]HNSWSearchResult, error) {
	return d.Graph.Search(query, topK)
}

// Search delegates to stitched search.
func (d *DiskANNIndex) Search(query []float32, topK int) ([]HNSWSearchResult, error) {
	return d.SearchDiskANN(query, topK)
}

func (d *DiskANNIndex) Len() int { return d.Graph.Len() }
func (d *DiskANNIndex) Close() error {
	return d.Graph.Close()
}

// ---------------------------------------------------------------------------
// GPU-accelerated DiskANN search (when CUDA is available)
// ---------------------------------------------------------------------------

type GPUQuery struct {
	Query []float32
	TopK  int
}

type GPUBatchResult struct {
	Results [][]HNSWSearchResult
}

// SearchGPUBatch performs batch stitched search on GPU.
// The GPU computes PQ-ADC for candidate expansion, CPU does final rerank.
func (d *DiskANNIndex) SearchGPUBatch(queries []GPUQuery, gpuBatchFn func(queries []GPUQuery, pqCodes [][]byte, dim int) ([][]HNSWSearchResult, error)) ([][]HNSWSearchResult, error) {
	if gpuBatchFn != nil {
		return gpuBatchFn(queries, d.PQCodes, d.Graph.dim)
	}
	// fallback to CPU
	results := make([][]HNSWSearchResult, len(queries))
	for i, q := range queries {
		r, err := d.SearchDiskANN(q.Query, q.TopK)
		if err != nil {
			return nil, err
		}
		results[i] = r
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Precomputed ADC helpers for use in GPU kernels
// ---------------------------------------------------------------------------

func (d *DiskANNIndex) PrecomputeADCTable(query []float32) [][]float32 {
	if d.PQ == nil {
		return nil
	}
	return d.PQ.PrecomputeQueryDistances(query)
}

const VamanaDistNorm = 0.0 // not used in ADC, just a marker
