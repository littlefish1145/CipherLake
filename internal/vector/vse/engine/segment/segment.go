package segment

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"nexus/internal/vector/vse"
	"nexus/internal/vector/vse/engine/cache"
	"nexus/internal/vector/vse/engine/index"
	"nexus/internal/vector/vse/engine/quantizer"
	"nexus/internal/vector/vse/gpu"
	"nexus/internal/vector/vse/simd"
	"nexus/internal/vector/vse/storage"
	"nexus/internal/vector/vse/storage/mmap"
)

type Segment struct {
	Meta        *vse.SegmentMeta
	Dir         string
	vectorsMMap *mmap.Reader
	vectorsData []byte
	hnswIndex   *index.HNSWIndex
	ivfPQIndex  *index.IVFPQIndex
	diskannIdx  *index.DiskANNIndex
	sqQuant     *quantizer.SQQuantizer
	pqQuant     *quantizer.PQQuantizer
	store       *storage.ObjectStore
	vecCache    *cache.Cache
	globalIDs   []uint64
	globalIDMap map[uint64]int
	gpuMgr      *gpu.Manager
	mu          sync.RWMutex
}

func NewSegment(meta *vse.SegmentMeta, dir string) *Segment {
	return &Segment{
		Meta:        meta,
		Dir:         dir,
		globalIDMap: make(map[uint64]int),
		vecCache:    cache.NewCache(1000),
	}
}

func (s *Segment) SetGPUManager(mgr *gpu.Manager) { s.gpuMgr = mgr }

func (s *Segment) GlobalIDs() []uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.globalIDs
}

func (s *Segment) Open(ctx context.Context, distFunc func(a, b []float32) float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	vecPath := filepath.Join(s.Dir, "vectors.bin")
	if _, err := os.Stat(vecPath); os.IsNotExist(err) {
		return fmt.Errorf("vectors.bin not found: %s", vecPath)
	}

	reader, err := mmap.Open(vecPath)
	if err != nil {
		return fmt.Errorf("mmap vectors: %w", err)
	}
	s.vectorsMMap = reader
	s.vectorsData = reader.Data()
	_ = ctx

	if s.Meta.Tier == vse.TierHot {
		graphPath := filepath.Join(s.Dir, "graph.bin")
		if _, err := os.Stat(graphPath); err == nil {
			idx, err := index.MmapFlatHNSWFromFunc(graphPath, distFunc)
			if err != nil {
				return fmt.Errorf("mmap graph: %w", err)
			}
			s.hnswIndex = index.WrapFlatHNSW(idx)
		}
	}

	vecsHeader := s.vectorsData[:8]
	n := int(binary.LittleEndian.Uint64(vecsHeader))
	if n != s.Meta.NumVectors && s.Meta.NumVectors > 0 {
		n = s.Meta.NumVectors
	}

	for i := 0; i < n; i++ {
		offset := 8 + i*8
		if offset+8 > len(s.vectorsData) {
			break
		}
		gid := binary.LittleEndian.Uint64(s.vectorsData[offset : offset+8])
		s.globalIDs = append(s.globalIDs, gid)
		s.globalIDMap[gid] = i
	}

	// Pin cold segment vectors + PQ data to GPU (best-effort, non-fatal)
	if s.gpuMgr != nil && s.gpuMgr.Enabled() && s.Meta.Tier == vse.TierCold && s.ivfPQIndex != nil {
		if err := s.gpuMgr.PinSegment(s.Meta, s.vectorsData, s.ivfPQIndex.PQ(), s.ivfPQIndex.IVF()); err != nil {
			fmt.Printf("[segment %d] gpu pin: %v (falling back to CPU)\n", s.Meta.ID, err)
		}
	}

	// Load DiskANN index for TierDiskANN segments
	if s.Meta.Tier == vse.TierDiskANN {
		idx, err := OpenDiskANNSegment(s.Dir, s.Meta, distFunc)
		if err != nil {
			return fmt.Errorf("open diskann: %w", err)
		}
		s.diskannIdx = idx

		if s.gpuMgr != nil && s.gpuMgr.Enabled() {
			nbrOffsets, nbrData := idx.Graph.Graph().NeighborArrays()
			if err := s.gpuMgr.PinVamanaGraph(s.Meta.ID, nbrOffsets, nbrData); err != nil {
				fmt.Printf("[segment %d] gpu pin vamana: %v (CPU fallback)\n", s.Meta.ID, err)
			}
		}
	}

	return nil
}

func (s *Segment) GetVector(localIdx int) []float32 {
	if localIdx < 0 || localIdx >= s.Meta.NumVectors {
		return nil
	}
	dim := s.Meta.Dimension
	if dim <= 0 {
		return nil
	}
	rowSize := 8 + dim*4
	offset := 8 + localIdx*rowSize
	if offset+dim*4 > len(s.vectorsData) {
		return nil
	}
	// 零拷贝：直接引用 mmap 内存（小端序平台，float32 布局与内存一致）
	// vectors.bin 格式: [8字节globalID][dim*4字节float32]
	// float32 数据起始偏移 = offset + 8
	vecStart := offset + 8
	return unsafe.Slice((*float32)(unsafe.Pointer(&s.vectorsData[vecStart])), dim)
}

// GetVectorInto 将 localIdx 处的向量拷贝到 dst，避免 heap 分配。
// dst 长度必须 >= dim。返回 dst[:dim]。
func (s *Segment) GetVectorInto(localIdx int, dst []float32) []float32 {
	if localIdx < 0 || localIdx >= s.Meta.NumVectors {
		return nil
	}
	dim := s.Meta.Dimension
	if dim <= 0 || len(dst) < dim {
		return nil
	}
	rowSize := 8 + dim*4
	offset := 8 + localIdx*rowSize
	if offset+8+dim*4 > len(s.vectorsData) {
		return nil
	}
	// 零拷贝引用 mmap 内存
	vecStart := offset + 8
	src := unsafe.Slice((*float32)(unsafe.Pointer(&s.vectorsData[vecStart])), dim)
	copy(dst[:dim], src)
	return dst[:dim]
}

func (s *Segment) GlobalIDToLocal(gid uint64) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx, ok := s.globalIDMap[gid]
	return idx, ok
}

func (s *Segment) Search(query []float32, topK int) ([]vse.SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Meta.Tier == vse.TierHot && s.hnswIndex != nil {
		results, err := s.hnswIndex.Search(query, topK)
		if err != nil {
			return nil, err
		}
		sr := make([]vse.SearchResult, 0, len(results))
		for _, r := range results {
			sr = append(sr, vse.SearchResult{
				ID:    vse.VectorID(r.ID),
				Score: r.Score,
				SegmentID: s.Meta.ID,
			})
		}
		return sr, nil
	}

	// GPU cold segment search (exact or PQ-accelerated)
	if s.gpuMgr != nil && s.gpuMgr.Enabled() && s.Meta.Tier == vse.TierCold {
		metric := vse.MetricEuclidean // default; caller can override via config
		results, err := s.gpuMgr.Search(s.Meta.ID, gpu.SearchRequest{
			Query:  query,
			TopK:   topK,
			Metric: metric,
		})
		if err == nil {
			for i := range results {
				results[i].SegmentID = s.Meta.ID
			}
			return results, nil
		}
		// GPU failed, fall through to CPU
	}

	if s.Meta.Tier == vse.TierCold && s.ivfPQIndex != nil {
		pqResults, err := s.ivfPQIndex.Search(query, s.globalIDs,
			func(localIdx int) []float32 { return s.GetVector(localIdx) }, topK)
		if err != nil {
			return nil, err
		}
		results := make([]vse.SearchResult, 0, len(pqResults))
		for _, r := range pqResults {
			results = append(results, vse.SearchResult{
				ID:    vse.VectorID(r.GlobalID),
				Score: r.FullDist,
				SegmentID: s.Meta.ID,
			})
		}
		return results, nil
	}

	// DiskANN stitched search (TierDiskANN)
	if s.Meta.Tier == vse.TierDiskANN && s.diskannIdx != nil {
		// GPU assisted Vamana search if available
		if s.gpuMgr != nil && s.gpuMgr.Enabled() && s.gpuMgr.HasVamanaGraph(s.Meta.ID) {
			candIDs, _, err := s.gpuMgr.VamanaSearchGPU(s.Meta.ID, query, topK, s.Meta.VamanaL)
			if err == nil {
				return s.rerankDiskANNCands(query, candIDs, topK)
			}
		}
		results, err := s.diskannIdx.SearchDiskANN(query, topK)
		if err != nil {
			return nil, err
		}
		sr := make([]vse.SearchResult, 0, len(results))
		for _, r := range results {
			sr = append(sr, vse.SearchResult{
				ID:        vse.VectorID(r.ID),
				Score:     r.Score,
				SegmentID: s.Meta.ID,
			})
		}
		return sr, nil
	}

	return s.flatSearch(query, topK)
}

func (s *Segment) flatSearch(query []float32, topK int) ([]vse.SearchResult, error) {
	type scoredIdx struct {
		idx  int
		dist float32
	}

	results := make([]scoredIdx, 0, s.Meta.NumVectors)
	for i := 0; i < s.Meta.NumVectors; i++ {
		vec := s.GetVector(i)
		if vec == nil {
			continue
		}
		// SIMD 加速的 L2 平方距离
		d := simd.L2Sq(vec, query)
		results = append(results, scoredIdx{idx: i, dist: d})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].dist < results[j].dist
	})
	if len(results) > topK {
		results = results[:topK]
	}

	sr := make([]vse.SearchResult, 0, len(results))
	for _, r := range results {
		sr = append(sr, vse.SearchResult{
			ID:    vse.VectorID(s.globalIDs[r.idx]),
			Score: r.dist,
			SegmentID: s.Meta.ID,
		})
	}
	return sr, nil
}

func (s *Segment) rerankDiskANNCands(query []float32, candIDs []int32, topK int) ([]vse.SearchResult, error) {
	type scored struct {
		id    uint64
		score float32
	}
	cands := make([]scored, 0, len(candIDs))
	g := s.diskannIdx.Graph.Graph()
	for _, nid := range candIDs {
		if int(nid) >= g.Len() {
			continue
		}
		gid := g.ID(nid)
		vec := g.NodeVector(nid)
		d := vse.L2DistanceSIMD(vec, query)
		cands = append(cands, scored{id: gid, score: d})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score < cands[j].score })
	if len(cands) > topK {
		cands = cands[:topK]
	}
	sr := make([]vse.SearchResult, len(cands))
	for i, c := range cands {
		sr[i] = vse.SearchResult{ID: vse.VectorID(c.id), Score: c.score, SegmentID: s.Meta.ID}
	}
	return sr, nil
}

func (s *Segment) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.vectorsMMap != nil {
		s.vectorsMMap.Close()
		s.vectorsMMap = nil
	}
	if s.hnswIndex != nil {
		s.hnswIndex.Close()
	}
	if s.diskannIdx != nil {
		s.diskannIdx.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
	if s.gpuMgr != nil && s.gpuMgr.Enabled() {
		_ = s.gpuMgr.UnpinSegment(s.Meta.ID)
	}
	return nil
}

type SegmentManager struct {
	mu           sync.RWMutex
	hotSegments  map[vse.SegmentID]*Segment
	coldSegments map[vse.SegmentID]*Segment
	baseDir      string
	distFunc     func(a, b []float32) float32
	gpuMgr       *gpu.Manager

	// Cold→Hot 自动提升：访问频率追踪
	coldAccessCount  map[vse.SegmentID]*atomic.Int64
	promoteThreshold atomic.Int64 // 访问次数阈值，超过则触发提升
	promoteMu        sync.Mutex  // 提升操作串行化
}

func NewSegmentManager(baseDir string, distFunc func(a, b []float32) float32, gpuMgr *gpu.Manager) *SegmentManager {
	sm := &SegmentManager{
		hotSegments:     make(map[vse.SegmentID]*Segment),
		coldSegments:    make(map[vse.SegmentID]*Segment),
		coldAccessCount: make(map[vse.SegmentID]*atomic.Int64),
		baseDir:         baseDir,
		distFunc:        distFunc,
		gpuMgr:          gpuMgr,
	}
	sm.promoteThreshold.Store(100) // 默认 100 次访问后触发提升
	return sm
}

func (sm *SegmentManager) LoadSegment(ctx context.Context, meta *vse.SegmentMeta) (*Segment, error) {
	segDir := filepath.Join(sm.baseDir, fmt.Sprintf("seg_%04d", meta.ID))
	seg := NewSegment(meta, segDir)
	seg.SetGPUManager(sm.gpuMgr)
	if err := seg.Open(ctx, sm.distFunc); err != nil {
		return nil, fmt.Errorf("load segment %d: %w", meta.ID, err)
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	if meta.Tier == vse.TierHot {
		sm.hotSegments[meta.ID] = seg
	} else {
		sm.coldSegments[meta.ID] = seg
		// 初始化访问计数器（TierDiskANN 也参与 Cold→Hot 提升）
		sm.coldAccessCount[meta.ID] = &atomic.Int64{}
	}
	return seg, nil
}

func (sm *SegmentManager) UnloadSegment(id vse.SegmentID) error {
	sm.mu.Lock()
	seg, ok := sm.hotSegments[id]
	if !ok {
		seg, ok = sm.coldSegments[id]
	}
	if !ok {
		sm.mu.Unlock()
		return fmt.Errorf("segment %d not found", id)
	}
	delete(sm.hotSegments, id)
	delete(sm.coldSegments, id)
	delete(sm.coldAccessCount, id)
	sm.mu.Unlock()
	return seg.Close()
}

// RecordColdAccess 记录冷段访问，达到阈值时异步触发 Cold→Hot 提升。
// 调用方应在每次搜索冷段后调用此方法。
func (sm *SegmentManager) RecordColdAccess(id vse.SegmentID) {
	sm.mu.RLock()
	counter, ok := sm.coldAccessCount[id]
	sm.mu.RUnlock()
	if !ok {
		return
	}
	count := counter.Add(1)
	threshold := sm.promoteThreshold.Load()
	if count == threshold {
		// 异步触发提升，避免阻塞搜索路径
		go sm.promoteColdSegment(id)
	}
}

// promoteColdSegment 将冷段提升为热段：构建 HNSW 索引并迁移到 hotSegments
func (sm *SegmentManager) promoteColdSegment(id vse.SegmentID) {
	sm.promoteMu.Lock()
	defer sm.promoteMu.Unlock()

	sm.mu.RLock()
	seg, ok := sm.coldSegments[id]
	sm.mu.RUnlock()
	if !ok {
		return // 已被卸载或已提升
	}

	// 构建热段目录
	hotDir := filepath.Join(sm.baseDir, fmt.Sprintf("seg_%04d_promoted", id))
	if err := os.MkdirAll(hotDir, 0755); err != nil {
		fmt.Printf("[segment %d] promote mkdir: %v\n", id, err)
		return
	}

	// 提取所有向量并写入新的热段格式
	dim := seg.Meta.Dimension
	n := seg.Meta.NumVectors
	entries := make([]vse.VectorEntry, 0, n)
	for i := 0; i < n; i++ {
		vec := seg.GetVector(i)
		if vec == nil {
			continue
		}
		gid := seg.globalIDs[i]
		entries = append(entries, vse.VectorEntry{
			ID:     vse.VectorID(gid),
			Values: vec,
		})
	}

	if len(entries) == 0 {
		fmt.Printf("[segment %d] promote: no vectors\n", id)
		return
	}

	promotedMeta := &vse.SegmentMeta{
		ID:         id,
		Tier:       vse.TierHot,
		NumVectors: len(entries),
		Dimension:  dim,
		CreatedAt:  seg.Meta.CreatedAt,
		UpdatedAt:  time.Now(),
		DataSize:   seg.Meta.DataSize,
	}

	if err := CreateSegmentOnDisk(hotDir, promotedMeta, entries); err != nil {
		fmt.Printf("[segment %d] promote create: %v\n", id, err)
		return
	}

	// 打开新的热段
	promotedSeg := NewSegment(promotedMeta, hotDir)
	promotedSeg.SetGPUManager(sm.gpuMgr)
	if err := promotedSeg.Open(context.Background(), sm.distFunc); err != nil {
		fmt.Printf("[segment %d] promote open: %v\n", id, err)
		return
	}

	// 原子替换：关闭旧冷段，注册新热段
	sm.mu.Lock()
	oldSeg, ok := sm.coldSegments[id]
	if !ok {
		// 竞态：已被其他 goroutine 处理
		sm.mu.Unlock()
		promotedSeg.Close()
		os.RemoveAll(hotDir)
		return
	}
	delete(sm.coldSegments, id)
	delete(sm.coldAccessCount, id)
	sm.hotSegments[id] = promotedSeg
	sm.mu.Unlock()

	// 关闭旧段（不删除 S3 数据，仅释放本地资源）
	oldSeg.Close()
	fmt.Printf("[segment %d] promoted cold→hot (%d vectors)\n", id, len(entries))
}

// SetPromoteThreshold 设置 Cold→Hot 提升的访问次数阈值
func (sm *SegmentManager) SetPromoteThreshold(threshold int64) {
	if threshold > 0 {
		sm.promoteThreshold.Store(threshold)
	}
}

func (sm *SegmentManager) GetSegment(id vse.SegmentID) *Segment {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if seg, ok := sm.hotSegments[id]; ok {
		return seg
	}
	if seg, ok := sm.coldSegments[id]; ok {
		return seg
	}
	return nil
}

func (sm *SegmentManager) ListHotSegments() []*Segment {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]*Segment, 0, len(sm.hotSegments))
	for _, seg := range sm.hotSegments {
		result = append(result, seg)
	}
	return result
}

func (sm *SegmentManager) ListColdSegments() []*Segment {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]*Segment, 0, len(sm.coldSegments))
	for _, seg := range sm.coldSegments {
		result = append(result, seg)
	}
	return result
}

func (sm *SegmentManager) TotalHotVectors() int64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	var total int64
	for _, seg := range sm.hotSegments {
		total += int64(seg.Meta.NumVectors)
	}
	return total
}

func (sm *SegmentManager) TotalColdVectors() int64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	var total int64
	for _, seg := range sm.coldSegments {
		total += int64(seg.Meta.NumVectors)
	}
	return total
}

func (sm *SegmentManager) Close() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for id, seg := range sm.hotSegments {
		seg.Close()
		delete(sm.hotSegments, id)
	}
	for id, seg := range sm.coldSegments {
		seg.Close()
		delete(sm.coldSegments, id)
	}
	return nil
}

func (sm *SegmentManager) MergeSegments(ctx context.Context, segs []*Segment, targetMeta *vse.SegmentMeta, distFunc func(a, b []float32) float32) (*Segment, error) {
	if len(segs) < 2 {
		return segs[0], nil
	}

	var allEntries []vse.VectorEntry
	var totalBytes int64
	dim := segs[0].Meta.Dimension

	for _, seg := range segs {
		for i := 0; i < seg.Meta.NumVectors; i++ {
			vec := seg.GetVector(i)
			if vec == nil {
				continue
			}
			gid := seg.globalIDs[i]
			allEntries = append(allEntries, vse.VectorEntry{
				ID:     vse.VectorID(gid),
				Values: vec,
			})
		}
		totalBytes += seg.Meta.DataSize
	}

	if len(allEntries) == 0 {
		return nil, fmt.Errorf("no vectors to merge")
	}

	var tier vse.Tier
	var prefix string
	if targetMeta.Tier == vse.TierHot {
		tier = vse.TierHot
		prefix = "hot"
	} else if targetMeta.Tier == vse.TierDiskANN {
		tier = vse.TierDiskANN
		prefix = "diskann"
	} else {
		tier = vse.TierCold
		prefix = "cold"
	}

	mergedMeta := &vse.SegmentMeta{
		ID:         targetMeta.ID,
		Tier:       tier,
		NumVectors: len(allEntries),
		Dimension:  dim,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
		DataSize:   totalBytes,
	}

	segDir := filepath.Join(sm.baseDir, fmt.Sprintf("seg_%s_%04d", prefix, targetMeta.ID))
	if err := CreateSegmentOnDisk(segDir, mergedMeta, allEntries); err != nil {
		return nil, fmt.Errorf("create merged segment: %w", err)
	}

	mergedSeg := NewSegment(mergedMeta, segDir)
	mergedSeg.SetGPUManager(sm.gpuMgr)
	if err := mergedSeg.Open(ctx, distFunc); err != nil {
		return nil, fmt.Errorf("open merged segment: %w", err)
	}

	sm.mu.Lock()
	if tier == vse.TierHot {
		sm.hotSegments[targetMeta.ID] = mergedSeg
	} else {
		sm.coldSegments[targetMeta.ID] = mergedSeg
	}
	sm.mu.Unlock()

	return mergedSeg, nil
}

func CreateSegmentOnDisk(dir string, meta *vse.SegmentMeta, entries []vse.VectorEntry) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create segment dir: %w", err)
	}

	dim := meta.Dimension
	n := len(entries)
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

	if meta.Tier == vse.TierHot {
		hnswIdx := index.NewHNSWIndex(dim, 16, 64, 0.5, func(a, b []float32) float32 {
			var dot, na, nb float64
			for i := range a {
				dot += float64(a[i]) * float64(b[i])
				na += float64(a[i]) * float64(a[i])
				nb += float64(b[i]) * float64(b[i])
			}
			if na == 0 || nb == 0 {
				return 1.0
			}
			return 1.0 - float32(dot/math.Sqrt(na*nb))
		})

		for _, e := range entries {
			if err := hnswIdx.Insert(uint64(e.ID), e.Values); err != nil {
				return fmt.Errorf("hnsw insert: %w", err)
			}
		}

		graphPath := filepath.Join(dir, "graph.bin")
		if err := hnswIdx.WriteFile(graphPath); err != nil {
			return fmt.Errorf("write graph.bin: %w", err)
		}
	}

	// DiskANN tier: build Vamana graph + PQ codes
	if meta.Tier == vse.TierDiskANN {
		// 使用包内的 CreateDiskANNSegment
		R := 32
		L := 64
		alpha := 1.2
		if meta.VamanaR > 0 {
			R = meta.VamanaR
		}
		if meta.VamanaL > 0 {
			L = meta.VamanaL
		}
		if meta.VamanaAlpha > 1.0 {
			alpha = meta.VamanaAlpha
		}
		if err := CreateDiskANNSegment(dir, meta, entries, R, L, alpha, vse.MetricEuclidean); err != nil {
			return fmt.Errorf("create diskann: %w", err)
		}
		return nil
	}

	metaPath := filepath.Join(dir, "meta.bin")
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := os.WriteFile(metaPath, metaJSON, 0644); err != nil {
		return fmt.Errorf("write meta.bin: %w", err)
	}

	return nil
}
