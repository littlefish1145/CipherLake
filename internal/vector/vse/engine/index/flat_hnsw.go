package index

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"
	"unsafe"

	"cipherlake/internal/vector/vse/simd"
	"cipherlake/internal/vector/vse/storage/mmap"
)

type searchScratch struct {
	candHeap  minHeap
	resultMax maxHeap
	visited   []int32
	visitGen  int32
	queryNorm float32
	queryPtr  unsafe.Pointer
	resultBuf []HNSWSearchResult
	withStats bool
	stats     HNSWSearchStats
}

// ---------------------------------------------------------------------------
// searchCandidate
// ---------------------------------------------------------------------------

type searchCandidate struct {
	nodeID int32
	dist   float32
}

// ---------------------------------------------------------------------------
// minHeap
// ---------------------------------------------------------------------------

type minHeap struct {
	data []searchCandidate
}

func (h *minHeap) Len() int           { return len(h.data) }
func (h *minHeap) Less(i, j int) bool { return h.data[i].dist < h.data[j].dist }
func (h *minHeap) Swap(i, j int)      { h.data[i], h.data[j] = h.data[j], h.data[i] }

func (h *minHeap) Push(x searchCandidate) {
	h.data = append(h.data, x)
	i := len(h.data) - 1
	for i > 0 {
		p := (i - 1) / 2
		if h.data[p].dist <= h.data[i].dist {
			break
		}
		h.data[p], h.data[i] = h.data[i], h.data[p]
		i = p
	}
}

func (h *minHeap) Pop() searchCandidate {
	n := len(h.data) - 1
	h.data[0], h.data[n] = h.data[n], h.data[0]
	i := 0
	for {
		left := 2*i + 1
		right := 2*i + 2
		best := i
		if left < n && h.data[left].dist < h.data[best].dist {
			best = left
		}
		if right < n && h.data[right].dist < h.data[best].dist {
			best = right
		}
		if best == i {
			break
		}
		h.data[i], h.data[best] = h.data[best], h.data[i]
		i = best
	}
	result := h.data[n]
	h.data = h.data[:n]
	return result
}

// ---------------------------------------------------------------------------
// maxHeap
// ---------------------------------------------------------------------------

type maxHeap struct {
	data []searchCandidate
}

func (h *maxHeap) Len() int           { return len(h.data) }
func (h *maxHeap) Less(i, j int) bool { return h.data[i].dist > h.data[j].dist }
func (h *maxHeap) Swap(i, j int)      { h.data[i], h.data[j] = h.data[j], h.data[i] }

func (h *maxHeap) Push(x searchCandidate) {
	h.data = append(h.data, x)
	i := len(h.data) - 1
	for i > 0 {
		p := (i - 1) / 2
		if h.data[p].dist >= h.data[i].dist {
			break
		}
		h.data[p], h.data[i] = h.data[i], h.data[p]
		i = p
	}
}

func (h *maxHeap) Pop() searchCandidate {
	n := len(h.data) - 1
	h.data[0], h.data[n] = h.data[n], h.data[0]
	i := 0
	for {
		left := 2*i + 1
		right := 2*i + 2
		best := i
		if left < n && h.data[left].dist > h.data[best].dist {
			best = left
		}
		if right < n && h.data[right].dist > h.data[best].dist {
			best = right
		}
		if best == i {
			break
		}
		h.data[i], h.data[best] = h.data[best], h.data[i]
		i = best
	}
	result := h.data[n]
	h.data = h.data[:n]
	return result
}

// ---------------------------------------------------------------------------
// MetricType — OPT 4: avoid func pointer
// ---------------------------------------------------------------------------

type MetricType int8

const (
	MetricCosine     MetricType = 0
	MetricEuclidean  MetricType = 1
	MetricDotProduct MetricType = 2
)

func distSIMDCosine(a, b []float32) float32 {
	dot := simd.Dot(a, b)
	na := simd.Dot(a, a)
	nb := simd.Dot(b, b)
	if na == 0 || nb == 0 {
		return 1.0
	}
	return 1.0 - dot/float32(math.Sqrt(float64(na*nb)))
}

func distSIMDEuclidean(a, b []float32) float32 {
	return simd.L2Sq(a, b)
}

func distSIMDDotProduct(a, b []float32) float32 {
	return 1.0 - simd.Dot(a, b)
}

// ---------------------------------------------------------------------------
// FlatHNSW
// ---------------------------------------------------------------------------

type FlatHNSW struct {
	dim                   int
	M                     int
	Mmax                  int
	Mmax0                 int
	Ml                    float64
	EfSearch              int
	EfConstruction        int
	metric                MetricType
	CheckRelativeDistance bool
	PruneHeadroom         float32
	rng                   *rand.Rand

	count    int
	capacity int
	levels   []int32
	vectors  []float32
	ids      []uint64

	neighbors [][]int32

	norms       []float32
	neighborBuf []int32
	distsBuf    []float32
	idxBuf      []int

	ep int32

	queryNorm float32

	candHeap  minHeap
	resultMax maxHeap
	visited   []int32
	visitGen  int32
	resultBuf []HNSWSearchResult

	scratchPool sync.Pool
	mmapReader  *mmap.Reader
	mu          sync.RWMutex
}

func NewFlatHNSWFromFunc(dim int, m int, efSearch int, ml float64, df func(a, b []float32) float32) *FlatHNSW {
	mt := detectMetric(df)
	if mt < 0 {
		mt = MetricCosine
	}
	return NewFlatHNSW(dim, m, efSearch, ml, mt)
}

func NewFlatHNSW(dim int, m int, efSearch int, ml float64, mt MetricType) *FlatHNSW {
	if m <= 0 {
		m = 16
	}
	if efSearch <= 0 {
		efSearch = 64
	}
	if ml <= 0 {
		ml = 0.5
	}

	initCap := 64
	bufSize := efSearch
	if bufSize < 64 {
		bufSize = 64
	}
	simd.Init()
	efCon := efSearch
	if efCon < 40 {
		efCon = 40
	}
	h := &FlatHNSW{
		dim:                   dim,
		M:                     m,
		Mmax:                  m * 2,
		Mmax0:                 m * 2,
		Ml:                    ml,
		EfSearch:              efSearch,
		EfConstruction:        efCon,
		metric:                mt,
		CheckRelativeDistance: false,
		PruneHeadroom:         0.0,
		rng:                   rand.New(rand.NewSource(time.Now().UnixNano())),
		count:                 0,
		capacity:              initCap,
		levels:                make([]int32, initCap),
		vectors:               make([]float32, initCap*dim),
		ids:                   make([]uint64, initCap),
		neighbors:             make([][]int32, initCap),
		norms:                 make([]float32, initCap),
		neighborBuf:           make([]int32, 0, m),
	}
	h.initScratchPool(bufSize)
	return h
}

// ---------------------------------------------------------------------------
// MmapFlatHNSW — reconstruct [][]int32 from flat mmap data via unsafe.Slice
// ---------------------------------------------------------------------------

func MmapFlatHNSW(path string, mt MetricType) (*FlatHNSW, error) {
	r, err := mmap.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mmap graph: %w", err)
	}
	data := r.Data()
	if len(data) < 28 {
		r.Close()
		return nil, fmt.Errorf("graph file too small: %d bytes", len(data))
	}

	off := 0
	read32 := func() uint32 {
		v := binary.LittleEndian.Uint32(data[off:])
		off += 4
		return v
	}
	read64 := func() uint64 {
		v := binary.LittleEndian.Uint64(data[off:])
		off += 8
		return v
	}

	count := int(read32())
	dim := int(read32())
	m := int(read32())
	ml := math.Float64frombits(read64())
	efSearch := int(read32())
	ep := int32(read32())

	levels := unsafe.Slice((*int32)(unsafe.Pointer(&data[off])), count)
	off += count * 4
	ids := unsafe.Slice((*uint64)(unsafe.Pointer(&data[off])), count)
	off += count * 8
	vectors := unsafe.Slice((*float32)(unsafe.Pointer(&data[off])), count*dim)
	off += count * dim * 4

	mmapOffsets := unsafe.Slice((*uint32)(unsafe.Pointer(&data[off])), count+1)
	off += (count + 1) * 4

	neighbors := make([][]int32, count)
	for i := 0; i < count; i++ {
		s := int(mmapOffsets[i])
		e := int(mmapOffsets[i+1])
		if s < e {
			ptr := (*int32)(unsafe.Pointer(&data[off+s*4]))
			neighbors[i] = unsafe.Slice(ptr, e-s)
		}
	}

	bufSize := efSearch
	if bufSize < 64 {
		bufSize = 64
	}
	h := &FlatHNSW{
		dim:            dim,
		M:              m,
		Mmax:           m,
		Mmax0:          m * 2,
		Ml:             ml,
		EfSearch:       efSearch,
		EfConstruction: efSearch,
		metric:         mt,
		ep:             ep,
		count:          count,
		capacity:       count,
		levels:         levels,
		vectors:        vectors,
		ids:            ids,
		neighbors:      neighbors,
		mmapReader:     r,
		candHeap:       minHeap{data: make([]searchCandidate, 0, bufSize)},
		resultMax:      maxHeap{data: make([]searchCandidate, 0, bufSize)},
		visited:        make([]int32, count),
		neighborBuf:    make([]int32, 0, m),
		distsBuf:       make([]float32, 0, m*2),
		idxBuf:         make([]int, 0, m*2),
	}
	h.initScratchPool(bufSize)
	initMmapNorms(h)
	return h, nil
}

func initMmapNorms(h *FlatHNSW) {
	simd.Init()
	if cap(h.norms) < h.count {
		h.norms = make([]float32, h.count)
	}
	h.norms = h.norms[:h.count]
	for i := 0; i < h.count; i++ {
		vec := h.nodeVector(int32(i))
		h.norms[i] = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
	}
}

func MmapFlatHNSWFromFunc(path string, df func(a, b []float32) float32) (*FlatHNSW, error) {
	mt := detectMetric(df)
	if mt < 0 {
		mt = MetricEuclidean
	}
	return MmapFlatHNSW(path, mt)
}

func detectMetric(df func(a, b []float32) float32) MetricType {
	a := []float32{2, 0, 0}
	b := []float32{1, 0, 0}
	d := df(a, b)
	if d >= -0.01 && d <= 0.01 {
		return MetricCosine
	}
	if d < 0 {
		return MetricDotProduct
	}
	return MetricEuclidean
}

func (h *FlatHNSW) Len() int { return h.count }

func (h *FlatHNSW) Dim() int { return h.dim }

func (h *FlatHNSW) initScratchPool(bufSize int) {
	h.scratchPool = sync.Pool{New: func() any {
		return &searchScratch{
			candHeap:  minHeap{data: make([]searchCandidate, 0, bufSize)},
			resultMax: maxHeap{data: make([]searchCandidate, 0, bufSize)},
			visited:   make([]int32, h.capacity),
			// 预分配 resultBuf 避免搜索热路径 heap 分配
			resultBuf: make([]HNSWSearchResult, 0, bufSize),
		}
	}}
}

func (h *FlatHNSW) recomputeNorms() {
	if cap(h.norms) < h.count {
		h.norms = make([]float32, h.count)
	}
	h.norms = h.norms[:h.count]
	simd.Init()
	for i := 0; i < h.count; i++ {
		vec := h.nodeVector(int32(i))
		h.norms[i] = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
	}
}

func (h *FlatHNSW) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mmapReader != nil {
		return h.mmapReader.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// distance — OPT 4: method dispatch, no func pointer
// ---------------------------------------------------------------------------

func (h *FlatHNSW) distance(a, b []float32) float32 {
	switch h.metric {
	case MetricEuclidean:
		return distSIMDEuclidean(a, b)
	case MetricDotProduct:
		return distSIMDDotProduct(a, b)
	default:
		return distSIMDCosine(a, b)
	}
}

func (h *FlatHNSW) distanceFromStored(storedNodeID int32, storedVec, queryVec []float32) float32 {
	switch h.metric {
	case MetricEuclidean:
		return simd.L2Sq(storedVec, queryVec)
	case MetricDotProduct:
		return 1.0 - simd.Dot(storedVec, queryVec)
	default:
		dot := simd.Dot(storedVec, queryVec)
		if dot == 0 {
			return 1.0
		}
		return 1.0 - dot/(h.norms[storedNodeID]*h.queryNorm)
	}
}

func (h *FlatHNSW) distanceFromStoredScratch(storedNodeID int32, storedVec, queryVec []float32, s *searchScratch) float32 {
	s.stats.DistanceCalls++
	switch h.metric {
	case MetricEuclidean:
		return simd.L2SqRaw(h.nodePtr(storedNodeID), s.queryPtr, h.dim)
	case MetricDotProduct:
		return 1.0 - simd.DotRaw(h.nodePtr(storedNodeID), s.queryPtr, h.dim)
	default:
		dot := simd.DotRaw(h.nodePtr(storedNodeID), s.queryPtr, h.dim)
		if dot == 0 {
			return 1.0
		}
		return 1.0 - dot/(h.norms[storedNodeID]*s.queryNorm)
	}
}

// distanceFromPtr 是 searchLayer / greedyDescend 所用的 raw-pointer 距离函数，
// 避免 nodeVector 创建 slice header。对 Euclidean 热路径跳过 switch。
func (h *FlatHNSW) distanceFromPtr(nodeID int32, queryPtr unsafe.Pointer, queryNorm float32) float32 {
	if h.metric == MetricEuclidean {
		return simd.L2SqRaw(h.nodePtr(nodeID), queryPtr, h.dim)
	}
	switch h.metric {
	case MetricDotProduct:
		return 1.0 - simd.DotRaw(h.nodePtr(nodeID), queryPtr, h.dim)
	default:
		dot := simd.DotRaw(h.nodePtr(nodeID), queryPtr, h.dim)
		if dot == 0 {
			return 1.0
		}
		return 1.0 - dot/(h.norms[nodeID]*queryNorm)
	}
}

// distanceRaw 是 searchLayerScratch / greedyDescendScratch 热路径专用内联距离。
// 它跳过 distanceFromStoredScratch 的函数调用/switch 开销，直接使用 raw kernel。
// 仅在 metric == MetricEuclidean 时可安全跳过 switch；其它 metric 回退到完整分发。
func (h *FlatHNSW) distanceRaw(nodeID int32, s *searchScratch) float32 {
	if h.metric == MetricEuclidean {
		return simd.L2SqRaw(h.nodePtr(nodeID), s.queryPtr, h.dim)
	}
	return h.distanceFromStoredScratch(nodeID, nil, nil, s)
}

// ---------------------------------------------------------------------------
// grow
// ---------------------------------------------------------------------------

func (h *FlatHNSW) grow() {
	newCap := h.capacity * 2
	if newCap < 64 {
		newCap = 64
	}

	newLevels := make([]int32, newCap)
	copy(newLevels, h.levels)
	h.levels = newLevels

	newVec := make([]float32, newCap*h.dim)
	copy(newVec, h.vectors)
	h.vectors = newVec

	newIDs := make([]uint64, newCap)
	copy(newIDs, h.ids)
	h.ids = newIDs

	newNeighbors := make([][]int32, newCap)
	copy(newNeighbors, h.neighbors)
	h.neighbors = newNeighbors

	newNorms := make([]float32, newCap)
	copy(newNorms, h.norms)
	h.norms = newNorms

	if newCap > len(h.visited) {
		newVis := make([]int32, newCap)
		copy(newVis, h.visited)
		h.visited = newVis
	}

	h.capacity = newCap
}

func (h *FlatHNSW) randomLevel() int {
	return int(math.Floor(math.Log(1.0-h.rng.Float64()) * (-h.Ml)))
}

func (h *FlatHNSW) nodeVector(idx int32) []float32 {
	return h.vectors[int(idx)*h.dim : (int(idx)+1)*h.dim]
}

func (h *FlatHNSW) nodePtr(idx int32) unsafe.Pointer {
	return unsafe.Pointer(&h.vectors[int(idx)*h.dim])
}

// ---------------------------------------------------------------------------
// visited — OPT 1: generation counter
// ---------------------------------------------------------------------------

func (h *FlatHNSW) visit(n int32) {
	if int(n) >= len(h.visited) {
		newV := make([]int32, max(len(h.visited)*2, int(n)+64))
		copy(newV, h.visited)
		h.visited = newV
	}
	h.visited[n] = h.visitGen
}

func (h *FlatHNSW) isVisited(n int32) bool {
	return int(n) < len(h.visited) && h.visited[n] == h.visitGen
}

func (h *FlatHNSW) visitScratch(s *searchScratch, n int32) {
	if int(n) >= len(s.visited) {
		newV := make([]int32, max(len(s.visited)*2, int(n)+64))
		copy(newV, s.visited)
		s.visited = newV
	}
	s.visited[n] = s.visitGen
}

func (h *FlatHNSW) isVisitedScratch(s *searchScratch, n int32) bool {
	return int(n) < len(s.visited) && s.visited[n] == s.visitGen
}

func (h *FlatHNSW) advanceScratchVisitGen(s *searchScratch) {
	if s.visitGen == math.MaxInt32 {
		clear(s.visited)
		s.visitGen = 1
		if s.withStats {
			s.stats.VisitedResets++
		}
		return
	}
	s.visitGen++
}

// ---------------------------------------------------------------------------
// neighbor access — always via neighbors [][]int32
// ---------------------------------------------------------------------------

func (h *FlatHNSW) getNeighborData(nodeID int32) []int32 {
	if int(nodeID) >= len(h.neighbors) {
		return nil
	}
	return h.neighbors[nodeID]
}

func (h *FlatHNSW) getLevelNeighbors(nodeID int32, level int) []int32 {
	data := h.getNeighborData(nodeID)
	if len(data) == 0 {
		return nil
	}
	numLevels := int(data[0])
	if level >= numLevels {
		return nil
	}
	off := 1
	for l := 0; l <= level; l++ {
		cnt := int(data[off])
		if l == level {
			if cnt == 0 {
				return nil
			}
			return data[off+1 : off+1+cnt]
		}
		off += 1 + cnt
	}
	return nil
}

func (h *FlatHNSW) setNodeNeighbors(nodeID int32, level int, nbrs []int32) {
	old := h.neighbors[nodeID]
	oldNumLevels := 0
	if len(old) > 0 {
		oldNumLevels = int(old[0])
	}
	numLevels := oldNumLevels
	if numLevels <= level {
		numLevels = level + 1
	}

	out := make([]int32, 0, 32)
	out = append(out, int32(numLevels))

	for l := 0; l < numLevels; l++ {
		if l == level {
			out = append(out, int32(len(nbrs)))
			out = append(out, nbrs...)
		} else if l < oldNumLevels {
			off := 1
			for i := 0; i < l; i++ {
				cnt := int(old[off])
				off += 1 + cnt
			}
			cnt := int(old[off])
			out = append(out, old[off:off+1+cnt]...)
		} else {
			out = append(out, 0)
		}
	}
	h.neighbors[nodeID] = out
}

func (h *FlatHNSW) addReverseConnection(nbrID int32, level int, nodeID int32) {
	if int(nbrID) >= len(h.neighbors) {
		return
	}
	if int(h.levels[nbrID]) < level {
		return
	}
	// layer 0 允许 2*M 条连接，上层允许 M 条
	maxNbrs := h.Mmax
	if level == 0 {
		maxNbrs = h.Mmax0
	}
	existing := h.getLevelNeighbors(nbrID, level)
	if len(existing) >= maxNbrs {
		all := make([]int32, 0, len(existing)+1)
		all = append(all, existing...)
		all = append(all, nodeID)
		pruned := h.pruneNeighborsHeuristic(nbrID, all, maxNbrs)
		h.setNodeNeighbors(nbrID, level, pruned)
		return
	}
	if cap(h.neighborBuf) < len(existing)+1 {
		h.neighborBuf = make([]int32, len(existing)+1)
	}
	h.neighborBuf = h.neighborBuf[:len(existing)+1]
	copy(h.neighborBuf, existing)
	h.neighborBuf[len(existing)] = nodeID
	h.setNodeNeighbors(nbrID, level, h.neighborBuf)
}

// ---------------------------------------------------------------------------
// searchLayer
// ---------------------------------------------------------------------------

func (h *FlatHNSW) searchLayer(queryPtr unsafe.Pointer, queryNorm float32, epID int32, level int, ef int, candidates *minHeap) {
	result := &h.resultMax
	result.data = result.data[:0]

	epDist := h.distanceFromPtr(epID, queryPtr, queryNorm)
	result.Push(searchCandidate{nodeID: epID, dist: epDist})
	candidates.Push(searchCandidate{nodeID: epID, dist: epDist})
	h.visit(epID)

	for candidates.Len() > 0 {
		c := candidates.Pop()
		worst := result.data[0].dist
		if c.dist > worst && result.Len() >= ef {
			break
		}
		neighbors := h.getLevelNeighbors(c.nodeID, level)
		for i, nbrID := range neighbors {
			if j := i + 2; j < len(neighbors) {
				simd.Prefetch(h.nodePtr(neighbors[j]), 0)
			}
			if h.isVisited(nbrID) {
				continue
			}
			h.visit(nbrID)
			d := h.distanceFromPtr(nbrID, queryPtr, queryNorm)
			if result.Len() >= ef && d >= result.data[0].dist {
				continue
			}
			cand := searchCandidate{nodeID: nbrID, dist: d}
			candidates.Push(cand)
			result.Push(cand)
			if result.Len() > ef {
				result.Pop()
			}
		}
	}
}

func (h *FlatHNSW) searchLayerScratch(queryVec []float32, epID int32, level int, ef int, candidates *minHeap, s *searchScratch) {
	result := &s.resultMax
	result.data = result.data[:0]

	epDist := h.distanceFromStoredScratch(epID, nil, nil, s)
	result.Push(searchCandidate{nodeID: epID, dist: epDist})
	candidates.Push(searchCandidate{nodeID: epID, dist: epDist})
	h.visitScratch(s, epID)

	for candidates.Len() > 0 {
		if s.withStats {
			s.stats.CandidatePops++
		}
		c := candidates.Pop()
		worst := result.data[0].dist
		if c.dist > worst && result.Len() >= ef {
			break
		}
		neighbors := h.getLevelNeighbors(c.nodeID, level)
		for i, nbrID := range neighbors {
			if s.withStats {
				s.stats.NeighborVisits++
			}
			if j := i + 2; j < len(neighbors) {
				simd.Prefetch(h.nodePtr(neighbors[j]), 0)
				if s.withStats {
					s.stats.VectorPrefetches++
				}
			}
			if h.isVisitedScratch(s, nbrID) {
				continue
			}
			h.visitScratch(s, nbrID)
			s.stats.DistanceCalls++
			d := h.distanceRaw(nbrID, s)
			if result.Len() >= ef && d >= result.data[0].dist {
				continue
			}
			cand := searchCandidate{nodeID: nbrID, dist: d}
			candidates.Push(cand)
			result.Push(cand)
			if result.Len() > ef {
				result.Pop()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// greedyDescend
// ---------------------------------------------------------------------------

func (h *FlatHNSW) greedyDescend(queryPtr unsafe.Pointer, queryNorm float32, epID int32, level int) int32 {
	ep := epID
	epDist := h.distanceFromPtr(ep, queryPtr, queryNorm)
	for {
		neighbors := h.getLevelNeighbors(ep, level)
		changed := false
		for i, nbrID := range neighbors {
			if j := i + 2; j < len(neighbors) {
				simd.Prefetch(h.nodePtr(neighbors[j]), 0)
			}
			d := h.distanceFromPtr(nbrID, queryPtr, queryNorm)
			if d < epDist {
				ep = nbrID
				epDist = d
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return ep
}

func (h *FlatHNSW) greedyDescendScratch(queryVec []float32, epID int32, level int, s *searchScratch) int32 {
	ep := epID
	epDist := h.distanceFromStoredScratch(ep, nil, nil, s)
	for {
		neighbors := h.getLevelNeighbors(ep, level)
		changed := false
		for i, nbrID := range neighbors {
			if s.withStats {
				s.stats.NeighborVisits++
			}
			if j := i + 2; j < len(neighbors) {
				simd.Prefetch(h.nodePtr(neighbors[j]), 0)
				if s.withStats {
					s.stats.VectorPrefetches++
				}
			}
			d := h.distanceRaw(nbrID, s)
			if d < epDist {
				ep = nbrID
				epDist = d
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return ep
}

// ---------------------------------------------------------------------------
// selectNeighbors / pruneNeighbors — OPT 2: manual sort, no closure
// ---------------------------------------------------------------------------

// nodeDist 用于邻居排序和启发式选择
type nodeDist struct {
	nodeID int32
	dist   float32
}

// selectNeighbors 使用 HNSW 论文 Algorithm 4 启发式选择，保证邻居多样性
func (h *FlatHNSW) selectNeighbors(results *maxHeap, m int) []int32 {
	n := results.Len()
	if n == 0 {
		return nil
	}

	// 将 maxHeap 数据拷贝出来按距离排序（距离最小的在前）
	sorted := make([]nodeDist, n)
	for i := 0; i < n; i++ {
		sorted[i] = nodeDist{nodeID: results.data[i].nodeID, dist: results.data[i].dist}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].dist < sorted[j].dist })

	// 使用简单选择（最近的 M 个），不使用启发式，避免丢弃有用连接
	return h.selectNeighborsSimple(sorted, m)
}

func (h *FlatHNSW) selectNeighborsSimple(candidates []nodeDist, m int) []int32 {
	need := m
	if len(candidates) < need {
		need = len(candidates)
	}
	out := make([]int32, need)
	for i := 0; i < need; i++ {
		out[i] = candidates[i].nodeID
	}
	return out
}

// heuristicSelect 实现 HNSW 论文 Algorithm 4 的启发式邻居选择
// 输入 candidates 已按到 query 的距离升序排列
// 对每个候选 e，仅当 e 到 query 的距离 < e 到任意已选邻居 r 的距离时才选中
func (h *FlatHNSW) heuristicSelect(candidates []nodeDist, m int) []int32 {
	relThresh := float32(1.0)
	if h.CheckRelativeDistance {
		relThresh = 1.0 + h.PruneHeadroom
	}
	need := len(candidates)
	if need > m {
		need = m
	}
	if need == 0 {
		return nil
	}

	result := make([]int32, 0, need)
	discarded := make([]nodeDist, 0, len(candidates))

	for _, c := range candidates {
		if len(result) >= m {
			break
		}
		good := true
		threshold := c.dist * relThresh
		cVec := h.nodeVector(c.nodeID)
		for _, rid := range result {
			rVec := h.nodeVector(rid)
			d := h.distance(cVec, rVec)
			if d < threshold {
				good = false
				break
			}
		}
		if good {
			result = append(result, c.nodeID)
		} else {
			discarded = append(discarded, c)
		}
	}

	// keepPrunedConnections: 如果还不够，从 discarded 中补充最近的
	for len(result) < m && len(discarded) > 0 {
		result = append(result, discarded[0].nodeID)
		discarded = discarded[1:]
	}

	if cap(h.neighborBuf) < len(result) {
		h.neighborBuf = make([]int32, len(result))
	}
	h.neighborBuf = h.neighborBuf[:len(result)]
	copy(h.neighborBuf, result)
	return h.neighborBuf
}

func (h *FlatHNSW) pruneNeighbors(nodeID int32, candidates []int32, maxNbrs int) []int32 {
	if len(candidates) <= maxNbrs {
		return candidates
	}

	n := len(candidates)
	vec := h.nodeVector(nodeID)

	if cap(h.distsBuf) < n {
		h.distsBuf = make([]float32, n)
	}
	h.distsBuf = h.distsBuf[:n]
	for i, nid := range candidates {
		h.distsBuf[i] = h.distance(vec, h.nodeVector(nid))
	}

	if cap(h.idxBuf) < n {
		h.idxBuf = make([]int, n)
	}
	h.idxBuf = h.idxBuf[:n]
	for i := range h.idxBuf {
		h.idxBuf[i] = i
	}
	sortIdxByDist(h.idxBuf, h.distsBuf)

	out := make([]int32, maxNbrs)
	for i := 0; i < maxNbrs; i++ {
		out[i] = candidates[h.idxBuf[i]]
	}
	return out
}

// pruneNeighborsHeuristic 使用启发式选择进行反向连接裁剪
func (h *FlatHNSW) pruneNeighborsHeuristic(nodeID int32, candidates []int32, maxNbrs int) []int32 {
	if len(candidates) <= maxNbrs {
		return candidates
	}

	n := len(candidates)
	vec := h.nodeVector(nodeID)

	relThresh := float32(1.0)
	if h.CheckRelativeDistance {
		relThresh = 1.0 + h.PruneHeadroom
	}

	// 计算所有候选到 nodeID 的距离
	sorted := make([]nodeDist, n)
	for i, nid := range candidates {
		sorted[i] = nodeDist{nodeID: nid, dist: h.distance(vec, h.nodeVector(nid))}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].dist < sorted[j].dist })

	// 启发式选择
	result := make([]int32, 0, maxNbrs)
	discarded := make([]nodeDist, 0, n)

	for _, c := range sorted {
		if len(result) >= maxNbrs {
			break
		}
		good := true
		threshold := c.dist * relThresh
		cVec := h.nodeVector(c.nodeID)
		for _, rid := range result {
			rVec := h.nodeVector(rid)
			d := h.distance(cVec, rVec)
			if d < threshold {
				good = false
				break
			}
		}
		if good {
			result = append(result, c.nodeID)
		} else {
			discarded = append(discarded, c)
		}
	}

	// keepPrunedConnections
	for len(result) < maxNbrs && len(discarded) > 0 {
		result = append(result, discarded[0].nodeID)
		discarded = discarded[1:]
	}

	return result
}

func sortIdxByDist(idx []int, dists []float32) {
	for i := 1; i < len(idx); i++ {
		v := idx[i]
		d := dists[v]
		j := i - 1
		for j >= 0 && dists[idx[j]] > d {
			idx[j+1] = idx[j]
			j--
		}
		idx[j+1] = v
	}
}

// ---------------------------------------------------------------------------
// Insert — OPT 3: per-node []int32, no flat shifting
// ---------------------------------------------------------------------------

func (h *FlatHNSW) Insert(id uint64, vec []float32) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(vec) != h.dim {
		return fmt.Errorf("dim mismatch: expected %d, got %d", h.dim, len(vec))
	}
	if h.count >= h.capacity {
		h.grow()
	}

	nodeID := int32(h.count)
	h.ids[nodeID] = id
	copy(h.nodeVector(nodeID), vec)
	h.norms[nodeID] = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
	lvl := h.randomLevel()
	h.levels[nodeID] = int32(lvl)
	h.count++
	h.queryNorm = float32(math.Sqrt(float64(simd.Dot(vec, vec))))

	if h.ep < 0 {
		h.ep = nodeID
		return nil
	}

	ep := h.ep
	epLevel := int(h.levels[ep])
	qp := unsafe.Pointer(&vec[0])

	h.visitGen++
	for level := epLevel; level > lvl; level-- {
		ep = h.greedyDescend(qp, h.queryNorm, ep, level)
	}
	for level := min(lvl, epLevel); level >= 0; level-- {
		h.visitGen++

		ef := h.EfConstruction
		if level == 0 && lvl == 0 {
			ef = max(ef, h.M)
		}

		h.candHeap.data = h.candHeap.data[:0]
		h.searchLayer(qp, h.queryNorm, ep, level, ef, &h.candHeap)

		// match FAISS: use closest result as entry for next lower level
		if level > 0 && h.resultMax.Len() > 0 {
			best := h.resultMax.data[0].nodeID
			bestDist := h.resultMax.data[0].dist
			for i := 1; i < h.resultMax.Len(); i++ {
				if h.resultMax.data[i].dist < bestDist {
					bestDist = h.resultMax.data[i].dist
					best = h.resultMax.data[i].nodeID
				}
			}
			ep = best
		}

		neighbors := h.selectNeighbors(&h.resultMax, h.M)
		h.setNodeNeighbors(nodeID, level, neighbors)

		for _, nid := range neighbors {
			h.addReverseConnection(nid, level, nodeID)
		}
	}

	if lvl > epLevel {
		h.ep = nodeID
	}

	return nil
}

// ---------------------------------------------------------------------------
// Build — OPT 5: batch insert, pre-alloc all slots
// ---------------------------------------------------------------------------

func (h *FlatHNSW) Build(ids []uint64, vectors []float32) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := len(ids)
	if n == 0 {
		return nil
	}
	if len(vectors) < n*h.dim {
		return fmt.Errorf("vectors too short: need %d, got %d", n*h.dim, len(vectors))
	}

	need := h.count + n
	for h.capacity < need {
		newCap := h.capacity * 2
		if newCap < need {
			newCap = need
		}

		newLevels := make([]int32, newCap)
		copy(newLevels, h.levels)
		h.levels = newLevels

		newVectors := make([]float32, newCap*h.dim)
		copy(newVectors, h.vectors)
		h.vectors = newVectors

		newIDs := make([]uint64, newCap)
		copy(newIDs, h.ids)
		h.ids = newIDs

		newNeighbors := make([][]int32, newCap)
		copy(newNeighbors, h.neighbors)
		h.neighbors = newNeighbors

		newNorms := make([]float32, newCap)
		copy(newNorms, h.norms)
		h.norms = newNorms

		h.capacity = newCap
	}

	base := h.count
	for i := 0; i < n; i++ {
		nodeID := base + i
		h.ids[nodeID] = ids[i]
		vec := vectors[i*h.dim : (i+1)*h.dim]
		copy(h.nodeVector(int32(nodeID)), vec)
		h.norms[nodeID] = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
		h.levels[nodeID] = int32(h.randomLevel())
	}
	h.count += n

	if h.ep < 0 {
		h.ep = int32(base)
	}

	for i := 0; i < n; i++ {
		nodeID := int32(base + i)
		lvl := int(h.levels[nodeID])
		if lvl < 0 {
			continue
		}

		ep := h.ep
		if i == 0 {
			continue
		}

		vec := vectors[i*h.dim : (i+1)*h.dim]
		h.queryNorm = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
		qp := unsafe.Pointer(&vec[0])
		epLevel := int(h.levels[ep])

		h.visitGen++
		for level := epLevel; level > lvl; level-- {
			ep = h.greedyDescend(qp, h.queryNorm, ep, level)
		}
		for level := min(lvl, epLevel); level >= 0; level-- {
			h.visitGen++

			ef := h.EfConstruction
			if level == 0 && lvl == 0 {
				ef = max(ef, h.M)
			}

			h.candHeap.data = h.candHeap.data[:0]
			h.searchLayer(qp, h.queryNorm, ep, level, ef, &h.candHeap)

			// match FAISS: use closest result as entry for next lower level
			if level > 0 && h.resultMax.Len() > 0 {
				best := h.resultMax.data[0].nodeID
				bestDist := h.resultMax.data[0].dist
				for i := 1; i < h.resultMax.Len(); i++ {
					if h.resultMax.data[i].dist < bestDist {
						bestDist = h.resultMax.data[i].dist
						best = h.resultMax.data[i].nodeID
					}
				}
				ep = best
			}

			neighbors := h.selectNeighbors(&h.resultMax, h.M)
			h.setNodeNeighbors(nodeID, level, neighbors)

			for _, nid := range neighbors {
				h.addReverseConnection(nid, level, nodeID)
			}
		}

		if lvl > epLevel {
			h.ep = nodeID
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func (h *FlatHNSW) Search(query []float32, topK int) ([]HNSWSearchResult, error) {
	s := h.scratchPool.Get().(*searchScratch)
	defer h.scratchPool.Put(s)

	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(query) != h.dim {
		return nil, fmt.Errorf("dim mismatch: expected %d, got %d", h.dim, len(query))
	}
	if h.count == 0 {
		return nil, nil
	}
	return h.searchWithScratch(query, topK, s)
}

func (h *FlatHNSW) SearchWithStats(query []float32, topK int) ([]HNSWSearchResult, HNSWSearchStats, error) {
	s := h.scratchPool.Get().(*searchScratch)
	defer h.scratchPool.Put(s)

	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(query) != h.dim {
		return nil, HNSWSearchStats{}, fmt.Errorf("dim mismatch: expected %d, got %d", h.dim, len(query))
	}
	if h.count == 0 {
		return nil, HNSWSearchStats{}, nil
	}
	s.withStats = true
	results, err := h.searchWithScratch(query, topK, s)
	s.withStats = false
	return results, s.stats, err
}

func (h *FlatHNSW) SearchWithScratch(query []float32, topK int, s *searchScratch) ([]HNSWSearchResult, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(query) != h.dim {
		return nil, fmt.Errorf("dim mismatch: expected %d, got %d", h.dim, len(query))
	}
	if h.count == 0 {
		return nil, nil
	}
	return h.searchWithScratch(query, topK, s)
}

func (h *FlatHNSW) searchWithScratch(query []float32, topK int, s *searchScratch) ([]HNSWSearchResult, error) {
	s.stats = HNSWSearchStats{}
	s.queryNorm = float32(math.Sqrt(float64(simd.Dot(query, query))))
	s.queryPtr = unsafe.Pointer(&query[0])

	ep := h.ep
	epLevel := int(h.levels[ep])

	h.advanceScratchVisitGen(s)
	var entryStart time.Time
	if s.withStats {
		entryStart = time.Now()
	}
	for level := epLevel; level > 0; level-- {
		ep = h.greedyDescendScratch(query, ep, level, s)
	}
	if s.withStats {
		s.stats.EntryDescendNs = time.Since(entryStart).Nanoseconds()
	}

	s.candHeap.data = s.candHeap.data[:0]
	var searchStart time.Time
	if s.withStats {
		searchStart = time.Now()
	}
	h.searchLayerScratch(query, ep, 0, h.EfSearch, &s.candHeap, s)
	if s.withStats {
		s.stats.LayerSearchNs = time.Since(searchStart).Nanoseconds()
	}

	all := s.resultMax.data
	topK = min(topK, len(all))
	var finalizeStart time.Time
	if s.withStats {
		finalizeStart = time.Now()
	}
	if topK > 0 && len(all) > 0 {
		if topK <= simd.MaxTopK {
			var sel simd.TopKSelector
			sel.Init(topK)
			for _, cand := range all {
				sel.Push(cand.dist, cand.nodeID)
				if s.withStats {
					s.stats.SelectorPushes++
				}
			}
			if cap(s.resultBuf) < topK {
				s.resultBuf = make([]HNSWSearchResult, topK)
			} else {
				s.resultBuf = s.resultBuf[:topK]
			}
			scores, idxs := sel.Result()
			for i, nodeID := range idxs {
				s.resultBuf[i] = HNSWSearchResult{
					ID:    h.ids[int(nodeID)],
					Value: h.nodeVector(nodeID),
					Score: scores[i],
				}
			}
			if s.withStats {
				s.stats.ResultFinalizeNs = time.Since(finalizeStart).Nanoseconds()
			}
			return s.resultBuf, nil
		}
		for i := 0; i < topK; i++ {
			best := i
			for j := i + 1; j < len(all); j++ {
				if all[j].dist < all[best].dist {
					best = j
				}
			}
			if best != i {
				all[i], all[best] = all[best], all[i]
			}
		}
	}
	all = all[:topK]

	// 复用 resultBuf 避免热路径 heap 分配
	// 容量已在 initScratchPool 预分配，仅在容量不足时才扩容
	if cap(s.resultBuf) < len(all) {
		s.resultBuf = make([]HNSWSearchResult, len(all))
	} else {
		s.resultBuf = s.resultBuf[:len(all)]
	}
	for i, c := range all {
		s.resultBuf[i] = HNSWSearchResult{
			ID:    h.ids[c.nodeID],
			Value: h.nodeVector(c.nodeID),
			Score: c.dist,
		}
	}
	if s.withStats {
		s.stats.ResultFinalizeNs = time.Since(finalizeStart).Nanoseconds()
	}
	return s.resultBuf, nil
}

// ---------------------------------------------------------------------------
// Serialize / WriteFile — flatten [][]int32 to mmap format
// ---------------------------------------------------------------------------

const _hnswHeader = 28

func (h *FlatHNSW) neighborTotal() int {
	n := 0
	for i := 0; i < h.count && i < len(h.neighbors); i++ {
		if h.neighbors[i] != nil {
			n += len(h.neighbors[i])
		}
	}
	return n
}

func (h *FlatHNSW) FileSize() int {
	return _hnswHeader + h.count*4 + h.count*8 + h.count*h.dim*4 + (h.count+1)*4 + h.neighborTotal()*4
}

func (h *FlatHNSW) flattenTo(data []byte) {
	off := 0
	put32 := func(v uint32) {
		binary.LittleEndian.PutUint32(data[off:], v)
		off += 4
	}
	put64 := func(v uint64) {
		binary.LittleEndian.PutUint64(data[off:], v)
		off += 8
	}

	put32(uint32(h.count))
	put32(uint32(h.dim))
	put32(uint32(h.M))
	put64(math.Float64bits(h.Ml))
	put32(uint32(h.EfSearch))
	put32(uint32(h.ep))

	for i := 0; i < h.count; i++ {
		put32(uint32(h.levels[i]))
	}
	for i := 0; i < h.count; i++ {
		put64(h.ids[i])
	}
	for i := 0; i < h.count*h.dim; i++ {
		put32(math.Float32bits(h.vectors[i]))
	}

	offOff := off
	off += (h.count + 1) * 4

	for i := 0; i < h.count; i++ {
		start := uint32((off - _hnswHeader - h.count*4 - h.count*8 - h.count*h.dim*4 - (h.count+1)*4) / 4)
		binary.LittleEndian.PutUint32(data[offOff+i*4:], start)

		if h.neighbors != nil && i < len(h.neighbors) && h.neighbors[i] != nil {
			for _, v := range h.neighbors[i] {
				put32(uint32(v))
			}
		}
	}
	binary.LittleEndian.PutUint32(data[offOff+h.count*4:], uint32((off-_hnswHeader-h.count*4-h.count*8-h.count*h.dim*4-(h.count+1)*4)/4))
}

func (h *FlatHNSW) WriteFile(path string) error {
	h.mu.RLock()
	data := make([]byte, h.FileSize())
	h.flattenTo(data)
	h.mu.RUnlock()
	return os.WriteFile(path, data, 0644)
}

func (h *FlatHNSW) Serialize() ([]byte, error) {
	h.mu.RLock()
	data := make([]byte, h.FileSize())
	h.flattenTo(data)
	h.mu.RUnlock()
	return data, nil
}

func (h *FlatHNSW) Deserialize(data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(data) < _hnswHeader {
		return fmt.Errorf("data too short: %d bytes", len(data))
	}
	off := 0
	read32 := func() uint32 {
		v := binary.LittleEndian.Uint32(data[off:])
		off += 4
		return v
	}
	read64 := func() uint64 {
		v := binary.LittleEndian.Uint64(data[off:])
		off += 8
		return v
	}

	count := int(read32())
	dim := int(read32())
	m := int(read32())
	ml := math.Float64frombits(read64())
	efSearch := int(read32())
	ep := int32(read32())

	h.dim = dim
	h.M = m
	h.Mmax = m
	h.Mmax0 = m * 2
	h.Ml = ml
	h.EfSearch = efSearch
	h.EfConstruction = efSearch
	h.ep = ep
	h.count = count
	h.capacity = count

	h.levels = make([]int32, count)
	for i := 0; i < count; i++ {
		h.levels[i] = int32(read32())
	}

	h.ids = make([]uint64, count)
	for i := 0; i < count; i++ {
		h.ids[i] = read64()
	}

	h.vectors = make([]float32, count*dim)
	for i := range h.vectors {
		h.vectors[i] = math.Float32frombits(read32())
	}

	offsOff := off
	_ = offsOff

	h.neighbors = make([][]int32, count)
	for i := 0; i < count; i++ {
		start := binary.LittleEndian.Uint32(data[offsOff+i*4:])
		end := binary.LittleEndian.Uint32(data[offsOff+(i+1)*4:])
		nlen := int(end - start)
		if nlen > 0 {
			nbrs := make([]int32, nlen)
			doff := _hnswHeader + count*4 + count*8 + count*dim*4 + (count+1)*4 + int(start)*4
			for j := range nbrs {
				nbrs[j] = int32(binary.LittleEndian.Uint32(data[doff+j*4:]))
			}
			h.neighbors[i] = nbrs
		}
	}

	h.neighborBuf = make([]int32, 0, h.M)
	h.distsBuf = make([]float32, 0, h.M*2)
	h.idxBuf = make([]int, 0, h.M*2)
	h.recomputeNorms()
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
