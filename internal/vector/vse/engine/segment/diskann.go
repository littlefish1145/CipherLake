package segment

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"cipherlake/internal/vector/vse"
	"cipherlake/internal/vector/vse/engine/index"
	"cipherlake/internal/vector/vse/engine/quantizer"
	"cipherlake/internal/vector/vse/gpu"
	"cipherlake/internal/vector/vse/simd"
)

// CreateDiskANNSegment 在磁盘上创建 DiskANN 段：Vamana 图 + PQ 码表 + 精确向量。
// dir: 段目录，meta: 段元数据（将被更新），entries: 向量条目，R/L/alpha: Vamana 参数，metric: 距离度量
func CreateDiskANNSegment(dir string, meta *vse.SegmentMeta, entries []vse.VectorEntry, R, L int, alpha float64, metric vse.MetricType) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create diskann dir: %w", err)
	}

	dim := meta.Dimension
	n := len(entries)
	if n == 0 {
		return fmt.Errorf("no vectors to create diskann segment")
	}

	M := meta.PQSubVecs
	if M <= 0 {
		M = dim / 4
		if M < 1 {
			M = 1
		}
	}
	bits := meta.PQBits
	if bits <= 0 {
		bits = 8
	}

	flatVecs := make([]float32, n*dim)
	for i, e := range entries {
		copy(flatVecs[i*dim:(i+1)*dim], e.Values)
	}

	pq := quantizer.NewPQQuantizer(dim, M, bits)
	if err := pq.TrainFromFlat(flatVecs, n); err != nil {
		return fmt.Errorf("pq train: %w", err)
	}

	pqCodes := make([]byte, n*M)
	for i := 0; i < n; i++ {
		code, err := pq.Encode(flatVecs[i*dim : (i+1)*dim])
		if err != nil {
			return fmt.Errorf("pq encode %d: %w", i, err)
		}
		copy(pqCodes[i*M:(i+1)*M], code)
	}

	mt := convertMetric(metric)
	graph := index.NewVamanaGraph(dim, R, L, alpha, mt)
	ids := make([]uint64, n)
	for i, e := range entries {
		ids[i] = uint64(e.ID)
	}
	if err := graph.Build(ids, flatVecs); err != nil {
		return fmt.Errorf("vamana build: %w", err)
	}

	// 写入 vectors.bin
	rowSize := 8 + dim*4
	totalSize := 8 + n*rowSize
	data := make([]byte, totalSize)
	binary.LittleEndian.PutUint64(data[:8], uint64(n))
	for i, e := range entries {
		row := 8 + i*rowSize
		binary.LittleEndian.PutUint64(data[row:row+8], uint64(e.ID))
		for j, f := range e.Values {
			if j >= dim {
				break
			}
			off := row + 8 + j*4
			binary.LittleEndian.PutUint32(data[off:off+4], math.Float32bits(f))
		}
	}
	vecPath := filepath.Join(dir, "vectors.bin")
	if err := os.WriteFile(vecPath, data, 0644); err != nil {
		return fmt.Errorf("write vectors.bin: %w", err)
	}

	// 写入 graph.bin
	graphPath := filepath.Join(dir, "graph.bin")
	if err := graph.WriteFile(graphPath); err != nil {
		return fmt.Errorf("write graph.bin: %w", err)
	}

	// 写入 pq_codes.bin
	pqCodePath := filepath.Join(dir, "pq_codes.bin")
	pqOut := make([]byte, 8+len(pqCodes))
	binary.LittleEndian.PutUint64(pqOut[:8], uint64(n))
	copy(pqOut[8:], pqCodes)
	if err := os.WriteFile(pqCodePath, pqOut, 0644); err != nil {
		return fmt.Errorf("write pq_codes.bin: %w", err)
	}

	// 写入 pq_centroids.bin
	centroids := pq.Centroids()
	subDim := pq.SubDim()
	cbFlat := make([]float32, M*256*subDim)
	for mm := 0; mm < M; mm++ {
		for k := 0; k < 256 && k < len(centroids[mm]); k++ {
			copy(cbFlat[(mm*256+k)*subDim:], centroids[mm][k][:subDim])
		}
	}
	cbPath := filepath.Join(dir, "pq_centroids.bin")
	cbOut := make([]byte, 12+len(cbFlat)*4)
	binary.LittleEndian.PutUint32(cbOut[0:4], uint32(M))
	binary.LittleEndian.PutUint32(cbOut[4:8], uint32(256))
	binary.LittleEndian.PutUint32(cbOut[8:12], uint32(subDim))
	for i, f := range cbFlat {
		binary.LittleEndian.PutUint32(cbOut[12+i*4:12+(i+1)*4], math.Float32bits(f))
	}
	if err := os.WriteFile(cbPath, cbOut, 0644); err != nil {
		return fmt.Errorf("write pq_centroids.bin: %w", err)
	}

	meta.VamanaR = R
	meta.VamanaL = L
	meta.VamanaAlpha = alpha
	meta.PQSubVecs = M
	meta.PQBits = bits
	meta.DataSize = int64(len(data))
	meta.CreatedAt = time.Now()
	meta.UpdatedAt = time.Now()
	meta.VectorsFile = vecPath
	meta.GraphFile = graphPath
	meta.PQFile = pqCodePath
	meta.CentroidFile = cbPath
	meta.MetaFile = filepath.Join(dir, "meta.bin")
	meta.NumVectors = n

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := os.WriteFile(meta.MetaFile, metaJSON, 0644); err != nil {
		return fmt.Errorf("write meta.bin: %w", err)
	}

	return nil
}

// OpenDiskANNSegment 打开已存在的 DiskANN 段。
func OpenDiskANNSegment(dir string, meta *vse.SegmentMeta, df func(a, b []float32) float32) (*index.DiskANNIndex, error) {
	dim := meta.Dimension
	R := meta.VamanaR
	if R <= 0 {
		R = 32
	}
	L := meta.VamanaL
	if L <= 0 {
		L = 64
	}
	alpha := meta.VamanaAlpha
	if alpha <= 1.0 {
		alpha = 1.2
	}

	pq, err := loadPQQuantizer(dir, meta)
	if err != nil {
		return nil, fmt.Errorf("load pq: %w", err)
	}

	pqCodes, err := loadPQCodes(dir, meta)
	if err != nil {
		return nil, fmt.Errorf("load pq codes: %w", err)
	}

	graphPath := filepath.Join(dir, "graph.bin")
	mt := convertMetricFromDist(df)
	g, err := index.MmapVamanaGraph(graphPath, mt)
	if err != nil {
		return nil, fmt.Errorf("mmap vamana graph: %w", err)
	}

	d := &index.DiskANNIndex{
		Graph:   index.WrapVamanaGraph(g),
		PQ:      pq,
		PQCodes: pqCodes,
		SimdFn:  df,
		Dim:     dim,
		Nodes:   meta.NumVectors,
	}

	return d, nil
}

// loadPQQuantizer 从磁盘加载 PQ 量化器
func loadPQQuantizer(dir string, meta *vse.SegmentMeta) (*quantizer.PQQuantizer, error) {
	cbPath := filepath.Join(dir, "pq_centroids.bin")
	cbData, err := os.ReadFile(cbPath)
	if err != nil {
		return nil, fmt.Errorf("read pq_centroids.bin: %w", err)
	}
	if len(cbData) < 12 {
		return nil, fmt.Errorf("pq_centroids.bin too short: %d", len(cbData))
	}
	M := int(binary.LittleEndian.Uint32(cbData[0:4]))
	K := int(binary.LittleEndian.Uint32(cbData[4:8]))
	subDim := int(binary.LittleEndian.Uint32(cbData[8:12]))
	_ = K

	if len(cbData) < 12+M*K*subDim*4 {
		return nil, fmt.Errorf("pq_centroids.bin truncated: %d < %d", len(cbData), 12+M*K*subDim*4)
	}

	centroids := make([][][]float32, M)
	for mm := 0; mm < M; mm++ {
		centroids[mm] = make([][]float32, 256)
		for k := 0; k < 256; k++ {
			vec := make([]float32, subDim)
			base := 12 + (mm*256+k)*subDim*4
			for j := 0; j < subDim; j++ {
				vec[j] = math.Float32frombits(binary.LittleEndian.Uint32(cbData[base+j*4:]))
			}
			centroids[mm][k] = vec
		}
	}

	bits := meta.PQBits
	if bits <= 0 {
		bits = 8
	}
	pq := quantizer.NewPQQuantizer(meta.Dimension, M, bits)
	pq.SetCentroids(centroids)
	return pq, nil
}

// loadPQCodes 从磁盘加载 PQ 编码
func loadPQCodes(dir string, meta *vse.SegmentMeta) ([][]byte, error) {
	pqPath := filepath.Join(dir, "pq_codes.bin")
	data, err := os.ReadFile(pqPath)
	if err != nil {
		return nil, fmt.Errorf("read pq_codes.bin: %w", err)
	}
	if len(data) < 8 {
		return nil, fmt.Errorf("pq_codes.bin too short")
	}
	n := int(binary.LittleEndian.Uint64(data[:8]))
	M := meta.PQSubVecs
	if M <= 0 {
		return nil, fmt.Errorf("PQSubVecs not set in meta")
	}
	if len(data) < 8+n*M {
		return nil, fmt.Errorf("pq_codes.bin truncated: %d < %d", len(data), 8+n*M)
	}

	codes := make([][]byte, n)
	for i := 0; i < n; i++ {
		code := make([]byte, M)
		copy(code, data[8+i*M:8+(i+1)*M])
		codes[i] = code
	}
	return codes, nil
}

// DiskANNSegment 提供与 Segment 兼容的搜索接口
type DiskANNSegment struct {
	Meta   *vse.SegmentMeta
	Dir    string
	Index  *index.DiskANNIndex
	gpuMgr *gpu.Manager
	mu     sync.RWMutex
}

func NewDiskANNSegment(meta *vse.SegmentMeta, dir string) *DiskANNSegment {
	return &DiskANNSegment{
		Meta: meta,
		Dir:  dir,
	}
}

func (ds *DiskANNSegment) SetGPUManager(mgr *gpu.Manager) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.gpuMgr = mgr
}

func (ds *DiskANNSegment) Open(ctx context.Context, df func(a, b []float32) float32) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	_ = ctx

	idx, err := OpenDiskANNSegment(ds.Dir, ds.Meta, df)
	if err != nil {
		return err
	}
	ds.Index = idx

	if ds.gpuMgr != nil && ds.gpuMgr.Enabled() {
		nbrOffsets, nbrData := idx.Graph.Graph().NeighborArrays()
		if err := ds.gpuMgr.PinVamanaGraph(ds.Meta.ID, nbrOffsets, nbrData); err != nil {
			fmt.Printf("[diskann %d] gpu pin vamana: %v (CPU fallback)\n", ds.Meta.ID, err)
		}
	}
	return nil
}

func (ds *DiskANNSegment) Search(query []float32, topK int) ([]vse.SearchResult, error) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	if ds.Index == nil {
		return nil, fmt.Errorf("diskann segment %d not opened", ds.Meta.ID)
	}

	if ds.gpuMgr != nil && ds.gpuMgr.Enabled() && ds.gpuMgr.HasVamanaGraph(ds.Meta.ID) {
		candIDs, _, err := ds.gpuMgr.VamanaSearchGPU(ds.Meta.ID, query, topK, ds.Meta.VamanaL)
		if err == nil {
			return ds.rerankFromCands(query, candIDs, topK)
		}
	}

	results, err := ds.Index.SearchDiskANN(query, topK)
	if err != nil {
		return nil, err
	}
	sr := make([]vse.SearchResult, 0, len(results))
	for _, r := range results {
		sr = append(sr, vse.SearchResult{
			ID:        vse.VectorID(r.ID),
			Score:     r.Score,
			SegmentID: ds.Meta.ID,
		})
	}
	return sr, nil
}

func (ds *DiskANNSegment) rerankFromCands(query []float32, candIDs []int32, topK int) ([]vse.SearchResult, error) {
	if topK <= 0 || len(candIDs) == 0 {
		return nil, nil
	}
	g := ds.Index.Graph.Graph()
	var sel simd.TopKSelector
	sel.Init(topK)
	for i, nid := range candIDs {
		if int(nid) >= g.Len() {
			continue
		}
		if j := i + 2; j < len(candIDs) {
			next := candIDs[j]
			if int(next) < g.Len() {
				vec := g.NodeVector(next)
				if len(vec) > 0 {
					simd.Prefetch(unsafe.Pointer(&vec[0]), 0)
				}
			}
		}
		vec := g.NodeVector(nid)
		d := ds.Index.SimdFn(vec, query)
		sel.Push(d, nid)
	}
	scores, idxs := sel.Result()
	sr := make([]vse.SearchResult, len(idxs))
	for i, nid := range idxs {
		sr[i] = vse.SearchResult{
			ID:        vse.VectorID(g.ID(nid)),
			Score:     scores[i],
			SegmentID: ds.Meta.ID,
		}
	}
	return sr, nil
}

func (ds *DiskANNSegment) Close() error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	var lastErr error
	if ds.Index != nil {
		if err := ds.Index.Close(); err != nil {
			lastErr = err
		}
	}
	if ds.gpuMgr != nil && ds.gpuMgr.Enabled() {
		_ = ds.gpuMgr.UnpinSegment(ds.Meta.ID)
	}
	return lastErr
}

func (ds *DiskANNSegment) GetVector(localIdx int) []float32 {
	if ds.Index == nil || ds.Index.Graph == nil {
		return nil
	}
	return ds.Index.Graph.Graph().NodeVector(int32(localIdx))
}

func (ds *DiskANNSegment) GlobalIDs() []uint64 {
	if ds.Index == nil || ds.Index.Graph == nil {
		return nil
	}
	return ds.Index.Graph.Graph().IDs()
}

func convertMetric(mt vse.MetricType) index.MetricType {
	switch mt {
	case vse.MetricEuclidean:
		return index.MetricEuclidean
	case vse.MetricDotProduct:
		return index.MetricDotProduct
	default:
		return index.MetricCosine
	}
}

func convertMetricFromDist(df func(a, b []float32) float32) index.MetricType {
	return index.MetricCosine
}

var _ = context.Background
