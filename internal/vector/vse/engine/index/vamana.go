package index

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"sync"
	"unsafe"

	"nexus/internal/vector/vse/simd"
	"nexus/internal/vector/vse/storage/mmap"
)

// VamanaGraph implements the Vamana graph index from the DiskANN paper.
// Unlike HNSW's multi-level hierarchy, Vamana uses a single-level graph
// with robust pruning (Algorithm 1) that ensures diversity among neighbors.
type VamanaGraph struct {
	dim    int
	R      int     // max out-degree
	L      int     // search list size (efSearch equivalent)
	alpha  float64 // pruning parameter (1 < α ≤ 2, typical 1.2)
	metric MetricType
	rng    *rand.Rand

	count    int
	capacity int
	vectors  []float32 // [capacity * dim]
	ids      []uint64  // [capacity]
	norms    []float32 // [capacity]

	neighbors [][]int32 // [capacity] flat neighbor lists (no level headers)

	ep int32 // entry point (medoid)

	// scratch buffers for build
	candHeap  minHeap
	resultMax maxHeap
	visited   []int32
	visitGen  int32
	resultBuf []HNSWSearchResult

	scratchPool sync.Pool
	mmapReader  *mmap.Reader
	mu          sync.RWMutex
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func NewVamanaGraph(dim, R, L int, alpha float64, mt MetricType) *VamanaGraph {
	if R <= 0 {
		R = 32
	}
	if L <= 0 {
		L = 64
	}
	if alpha <= 1 {
		alpha = 1.2
	}

	initCap := 64
	bufSize := L
	if bufSize < 64 {
		bufSize = 64
	}
	simd.Init()

	return &VamanaGraph{
		dim:       dim,
		R:         R,
		L:         L,
		alpha:     alpha,
		metric:    mt,
		rng:       rand.New(rand.NewSource(42)),
		ep:        -1,
		capacity:  initCap,
		vectors:   make([]float32, initCap*dim),
		ids:       make([]uint64, initCap),
		norms:     make([]float32, initCap),
		neighbors: make([][]int32, initCap),
		candHeap:  minHeap{data: make([]searchCandidate, 0, bufSize)},
		resultMax: maxHeap{data: make([]searchCandidate, 0, bufSize)},
		visited:   make([]int32, initCap),
		scratchPool: sync.Pool{New: func() any {
			return &searchScratch{
				candHeap:  minHeap{data: make([]searchCandidate, 0, bufSize)},
				resultMax: maxHeap{data: make([]searchCandidate, 0, bufSize)},
				visited:   make([]int32, initCap),
				resultBuf: make([]HNSWSearchResult, 0, bufSize),
			}
		}},
	}
}

// ---------------------------------------------------------------------------
// mmap
// ---------------------------------------------------------------------------

func MmapVamanaGraph(path string, mt MetricType) (*VamanaGraph, error) {
	r, err := mmap.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mmap vamana: %w", err)
	}
	data := r.Data()
	if len(data) < 28 {
		r.Close()
		return nil, fmt.Errorf("vamana file too small: %d bytes", len(data))
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
	R := int(read32())
	L := int(read32())
	alpha := math.Float64frombits(read64())
	ep := int32(read32())

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

	bufSize := L
	if bufSize < 64 {
		bufSize = 64
	}

	g := &VamanaGraph{
		dim:        dim,
		R:          R,
		L:          L,
		alpha:      alpha,
		metric:     mt,
		count:      count,
		capacity:   count,
		ep:         ep,
		ids:        ids,
		vectors:    vectors,
		neighbors:  neighbors,
		mmapReader: r,
		candHeap:   minHeap{data: make([]searchCandidate, 0, bufSize)},
		resultMax:  maxHeap{data: make([]searchCandidate, 0, bufSize)},
		visited:    make([]int32, count),
	}
	g.scratchPool = sync.Pool{New: func() any {
		return &searchScratch{
			candHeap:  minHeap{data: make([]searchCandidate, 0, bufSize)},
			resultMax: maxHeap{data: make([]searchCandidate, 0, bufSize)},
			visited:   make([]int32, count),
			resultBuf: make([]HNSWSearchResult, 0, bufSize),
		}
	}}
	initVamanaNorms(g)
	return g, nil
}

func initVamanaNorms(g *VamanaGraph) {
	simd.Init()
	if cap(g.norms) < g.count {
		g.norms = make([]float32, g.count)
	}
	g.norms = g.norms[:g.count]
	for i := 0; i < g.count; i++ {
		vec := g.nodeVector(int32(i))
		g.norms[i] = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
	}
}

// ---------------------------------------------------------------------------
// basic accessors
// ---------------------------------------------------------------------------

func (g *VamanaGraph) Len() int          { return g.count }
func (g *VamanaGraph) Dim() int          { return g.dim }
func (g *VamanaGraph) EntryPoint() int32 { return g.ep }

// IDs 返回所有节点 ID 的切片
func (g *VamanaGraph) IDs() []uint64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ids := make([]uint64, g.count)
	copy(ids, g.ids[:g.count])
	return ids
}

// ID 返回指定本地索引的全局 ID
func (g *VamanaGraph) ID(localIdx int32) uint64 {
	if int(localIdx) >= g.count {
		return 0
	}
	return g.ids[localIdx]
}

// NodeVector 返回指定节点的向量（零拷贝引用）
func (g *VamanaGraph) NodeVector(localIdx int32) []float32 {
	if int(localIdx) >= g.count {
		return nil
	}
	return g.nodeVector(localIdx)
}

func (g *VamanaGraph) VectorsBase() unsafe.Pointer {
	if len(g.vectors) == 0 {
		return nil
	}
	return unsafe.Pointer(&g.vectors[0])
}

// NeighborArrays 返回 CSR 格式的邻居数组，用于 GPU 上传或分析。
// offsets[i] = 节点 i 的邻居在 data 中的起始偏移；offsets[n] = 总邻居数。
func (g *VamanaGraph) NeighborArrays() (offsets []int32, data []int32) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	offsets = make([]int32, g.count+1)
	total := 0
	for i := 0; i < g.count; i++ {
		offsets[i] = int32(total)
		if i < len(g.neighbors) && g.neighbors[i] != nil {
			total += len(g.neighbors[i])
		}
	}
	offsets[g.count] = int32(total)
	data = make([]int32, total)
	off := 0
	for i := 0; i < g.count; i++ {
		if i < len(g.neighbors) && g.neighbors[i] != nil {
			copy(data[off:], g.neighbors[i])
			off += len(g.neighbors[i])
		}
	}
	return
}

func (g *VamanaGraph) nodeVector(idx int32) []float32 {
	return g.vectors[int(idx)*g.dim : (int(idx)+1)*g.dim]
}

// ---------------------------------------------------------------------------
// visited (generation counter)
// ---------------------------------------------------------------------------

func (g *VamanaGraph) visit(n int32) {
	if int(n) >= len(g.visited) {
		newV := make([]int32, max(len(g.visited)*2, int(n)+64))
		copy(newV, g.visited)
		g.visited = newV
	}
	g.visited[n] = g.visitGen
}

func (g *VamanaGraph) isVisited(n int32) bool {
	return int(n) < len(g.visited) && g.visited[n] == g.visitGen
}

func (g *VamanaGraph) visitScratch(s *searchScratch, n int32) {
	if int(n) >= len(s.visited) {
		newV := make([]int32, max(len(s.visited)*2, int(n)+64))
		copy(newV, s.visited)
		s.visited = newV
	}
	s.visited[n] = s.visitGen
}

func (g *VamanaGraph) isVisitedScratch(s *searchScratch, n int32) bool {
	return int(n) < len(s.visited) && s.visited[n] == s.visitGen
}

// ---------------------------------------------------------------------------
// distance dispatch
// ---------------------------------------------------------------------------

func (g *VamanaGraph) distance(a, b []float32) float32 {
	switch g.metric {
	case MetricEuclidean:
		return simd.L2Sq(a, b)
	case MetricDotProduct:
		return 1.0 - simd.Dot(a, b)
	default:
		return distSIMDCosine(a, b)
	}
}

func (g *VamanaGraph) distanceFromStored(storedNodeID int32, storedVec, queryVec []float32) float32 {
	switch g.metric {
	case MetricEuclidean:
		return simd.L2Sq(storedVec, queryVec)
	case MetricDotProduct:
		return 1.0 - simd.Dot(storedVec, queryVec)
	default:
		dot := simd.Dot(storedVec, queryVec)
		if dot == 0 {
			return 1.0
		}
		return 1.0 - dot/(g.norms[storedNodeID]*float32(math.Sqrt(float64(simd.Dot(queryVec, queryVec)))))
	}
}

func (g *VamanaGraph) distanceFromStoredScratch(storedNodeID int32, storedVec, queryVec []float32, s *searchScratch) float32 {
	switch g.metric {
	case MetricEuclidean:
		return simd.L2Sq(storedVec, queryVec)
	case MetricDotProduct:
		return 1.0 - simd.Dot(storedVec, queryVec)
	default:
		dot := simd.Dot(storedVec, queryVec)
		if dot == 0 || s.queryNorm == 0 {
			return 1.0
		}
		return 1.0 - dot/(g.norms[storedNodeID]*s.queryNorm)
	}
}

// ---------------------------------------------------------------------------
// grow
// ---------------------------------------------------------------------------

func (g *VamanaGraph) grow() {
	newCap := g.capacity * 2
	if newCap < 64 {
		newCap = 64
	}

	newIDs := make([]uint64, newCap)
	copy(newIDs, g.ids)
	g.ids = newIDs

	newVec := make([]float32, newCap*g.dim)
	copy(newVec, g.vectors)
	g.vectors = newVec

	newNbrs := make([][]int32, newCap)
	copy(newNbrs, g.neighbors)
	g.neighbors = newNbrs

	newNorms := make([]float32, newCap)
	copy(newNorms, g.norms)
	g.norms = newNorms

	if newCap > len(g.visited) {
		newVis := make([]int32, newCap)
		copy(newVis, g.visited)
		g.visited = newVis
	}

	g.capacity = newCap
}

// ---------------------------------------------------------------------------
// GreedySearch — find closest single node at a given level
// ---------------------------------------------------------------------------

func (g *VamanaGraph) greedyDescend(queryVec []float32, epID int32) int32 {
	ep := epID
	epDist := g.distanceFromStored(ep, g.nodeVector(ep), queryVec)
	for {
		neighbors := g.neighbors[ep]
		changed := false
		for _, nbrID := range neighbors {
			d := g.distanceFromStored(nbrID, g.nodeVector(nbrID), queryVec)
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

func (g *VamanaGraph) greedyDescendScratch(queryVec []float32, epID int32, s *searchScratch) int32 {
	ep := epID
	epDist := g.distanceFromStoredScratch(ep, g.nodeVector(ep), queryVec, s)
	for {
		neighbors := g.neighbors[ep]
		changed := false
		for _, nbrID := range neighbors {
			d := g.distanceFromStoredScratch(nbrID, g.nodeVector(nbrID), queryVec, s)
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
// BeamSearch — find L nearest nodes, fills candidates & result
// ---------------------------------------------------------------------------

func (g *VamanaGraph) beamSearch(queryVec []float32, epID int32, L int) {
	result := &g.resultMax
	result.data = result.data[:0]
	candidates := &g.candHeap
	candidates.data = candidates.data[:0]

	epDist := g.distanceFromStored(epID, g.nodeVector(epID), queryVec)
	g.visitGen++
	g.visit(epID)
	candidates.Push(searchCandidate{nodeID: epID, dist: epDist})
	result.Push(searchCandidate{nodeID: epID, dist: epDist})

	for candidates.Len() > 0 {
		c := candidates.Pop()
		worst := result.data[0].dist
		if c.dist > worst && result.Len() >= L {
			break
		}
		for _, nbrID := range g.neighbors[c.nodeID] {
			if g.isVisited(nbrID) {
				continue
			}
			g.visit(nbrID)
			d := g.distanceFromStored(nbrID, g.nodeVector(nbrID), queryVec)
			if result.Len() >= L && d >= result.data[0].dist {
				continue
			}
			cand := searchCandidate{nodeID: nbrID, dist: d}
			candidates.Push(cand)
			result.Push(cand)
			if result.Len() > L {
				result.Pop()
			}
		}
	}
}

func (g *VamanaGraph) beamSearchScratch(queryVec []float32, epID int32, L int, s *searchScratch) {
	result := &s.resultMax
	result.data = result.data[:0]
	candidates := &s.candHeap
	candidates.data = candidates.data[:0]

	epDist := g.distanceFromStoredScratch(epID, g.nodeVector(epID), queryVec, s)
	s.visitGen++
	g.visitScratch(s, epID)
	candidates.Push(searchCandidate{nodeID: epID, dist: epDist})
	result.Push(searchCandidate{nodeID: epID, dist: epDist})

	for candidates.Len() > 0 {
		c := candidates.Pop()
		worst := result.data[0].dist
		if c.dist > worst && result.Len() >= L {
			break
		}
		for _, nbrID := range g.neighbors[c.nodeID] {
			if g.isVisitedScratch(s, nbrID) {
				continue
			}
			g.visitScratch(s, nbrID)
			d := g.distanceFromStoredScratch(nbrID, g.nodeVector(nbrID), queryVec, s)
			if result.Len() >= L && d >= result.data[0].dist {
				continue
			}
			cand := searchCandidate{nodeID: nbrID, dist: d}
			candidates.Push(cand)
			result.Push(cand)
			if result.Len() > L {
				result.Pop()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// RobustPrune — DiskANN Algorithm 1
//
// Given a node p and candidate set V (neighbors), select up to R neighbors
// with a diversity guarantee: a candidate is kept only if it is not closer
// to any already-selected neighbor than to p (scaled by α).
// ---------------------------------------------------------------------------

type vamanaNodeDist struct {
	nodeID int32
	dist   float32
}

func (g *VamanaGraph) robustPrune(p int32, candidates []int32, R int) []int32 {
	if len(candidates) <= R {
		out := make([]int32, len(candidates))
		copy(out, candidates)
		return out
	}

	pVec := g.nodeVector(p)
	n := len(candidates)
	sorted := make([]vamanaNodeDist, n)
	for i, nid := range candidates {
		sorted[i] = vamanaNodeDist{nodeID: nid, dist: g.distance(pVec, g.nodeVector(nid))}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].dist < sorted[j].dist })

	result := make([]int32, 0, R)
	discarded := make([]vamanaNodeDist, 0, n)

	for _, c := range sorted {
		if len(result) >= R {
			break
		}
		good := true
		cVec := g.nodeVector(c.nodeID)
		for _, rid := range result {
			rVec := g.nodeVector(rid)
			d := g.distance(cVec, rVec)
			if d*float32(g.alpha) < c.dist {
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

	for len(result) < R && len(discarded) > 0 {
		result = append(result, discarded[0].nodeID)
		discarded = discarded[1:]
	}

	return result
}

// ---------------------------------------------------------------------------
// Insert — single point insertion with full Vamana build step
// ---------------------------------------------------------------------------

func (g *VamanaGraph) Insert(id uint64, vec []float32) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(vec) != g.dim {
		return fmt.Errorf("dim mismatch: expected %d, got %d", g.dim, len(vec))
	}
	if g.count >= g.capacity {
		g.grow()
	}

	nodeID := int32(g.count)
	g.ids[nodeID] = id
	copy(g.nodeVector(nodeID), vec)
	g.norms[nodeID] = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
	g.count++

	if g.ep < 0 {
		g.ep = nodeID
		g.neighbors[nodeID] = []int32{}
		return nil
	}

	// Step a: find closest starting point
	ep := g.greedyDescend(vec, g.ep)

	// Step b: find L nearest neighbors
	g.beamSearch(vec, ep, g.L)
	candidates := g.resultMax.data
	candIDs := make([]int32, len(candidates))
	for i, c := range candidates {
		candIDs[i] = c.nodeID
	}

	// Step c: robust prune
	nbrs := g.robustPrune(nodeID, candIDs, g.R)
	g.neighbors[nodeID] = nbrs

	// Step d: update reverse edges
	for _, nid := range nbrs {
		existing := g.neighbors[nid]
		if len(existing) >= g.R {
			combined := make([]int32, 0, len(existing)+1)
			combined = append(combined, existing...)
			combined = append(combined, nodeID)
			g.neighbors[nid] = g.robustPrune(nid, combined, g.R)
		} else {
			newNbrs := make([]int32, len(existing)+1)
			copy(newNbrs, existing)
			newNbrs[len(existing)] = nodeID
			g.neighbors[nid] = newNbrs
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Build — batch insert
// ---------------------------------------------------------------------------

func (g *VamanaGraph) Build(ids []uint64, vectors []float32) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	n := len(ids)
	if n == 0 {
		return nil
	}
	if len(vectors) < n*g.dim {
		return fmt.Errorf("vectors too short: need %d, got %d", n*g.dim, len(vectors))
	}

	for g.capacity < g.count+n {
		newCap := g.capacity * 2
		if newCap < g.count+n {
			newCap = g.count + n
		}

		newIDs := make([]uint64, newCap)
		copy(newIDs, g.ids)
		g.ids = newIDs

		newVec := make([]float32, newCap*g.dim)
		copy(newVec, g.vectors)
		g.vectors = newVec

		newNbrs := make([][]int32, newCap)
		copy(newNbrs, g.neighbors)
		g.neighbors = newNbrs

		newNorms := make([]float32, newCap)
		copy(newNorms, g.norms)
		g.norms = newNorms

		g.capacity = newCap
	}

	base := g.count
	for i := 0; i < n; i++ {
		nodeID := base + i
		g.ids[nodeID] = ids[i]
		vec := vectors[i*g.dim : (i+1)*g.dim]
		copy(g.nodeVector(int32(nodeID)), vec)
		g.norms[nodeID] = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
	}
	g.count += n

	// Set entry point on first build
	if g.ep < 0 {
		g.ep = int32(base)
	}
	// Initialize neighbors for the first node of this batch
	g.neighbors[base] = []int32{}
	// Cannot build edges with only one node
	if n <= 1 {
		return nil
	}

	for i := 1; i < n; i++ {
		nodeID := int32(base + i)

		vec := vectors[i*g.dim : (i+1)*g.dim]
		ep := g.greedyDescend(vec, g.ep)
		g.beamSearch(vec, ep, g.L)

		candIDs := make([]int32, len(g.resultMax.data))
		for j, c := range g.resultMax.data {
			candIDs[j] = c.nodeID
		}

		nbrs := g.robustPrune(nodeID, candIDs, g.R)
		g.neighbors[nodeID] = nbrs

		for _, nid := range nbrs {
			existing := g.neighbors[nid]
			if len(existing) >= g.R {
				combined := make([]int32, 0, len(existing)+1)
				combined = append(combined, existing...)
				combined = append(combined, nodeID)
				g.neighbors[nid] = g.robustPrune(nid, combined, g.R)
			} else {
				newNbrs := make([]int32, len(existing)+1)
				copy(newNbrs, existing)
				newNbrs[len(existing)] = nodeID
				g.neighbors[nid] = newNbrs
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Search — exact distance search
// ---------------------------------------------------------------------------

func (g *VamanaGraph) Search(query []float32, topK int) ([]HNSWSearchResult, error) {
	s := g.scratchPool.Get().(*searchScratch)
	defer g.scratchPool.Put(s)

	g.mu.RLock()
	defer g.mu.RUnlock()

	if len(query) != g.dim {
		return nil, fmt.Errorf("dim mismatch: expected %d, got %d", g.dim, len(query))
	}
	if g.count == 0 {
		return nil, nil
	}

	return g.searchWithScratch(query, topK, s)
}

func (g *VamanaGraph) searchWithScratch(query []float32, topK int, s *searchScratch) ([]HNSWSearchResult, error) {
	s.queryNorm = float32(math.Sqrt(float64(simd.Dot(query, query))))
	ep := g.greedyDescendScratch(query, g.ep, s)
	g.beamSearchScratch(query, ep, g.L, s)

	all := s.resultMax.data
	topK = min(topK, len(all))

	// selection sort to get top-K by ascending distance
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
	all = all[:topK]

	if cap(s.resultBuf) < len(all) {
		s.resultBuf = make([]HNSWSearchResult, len(all))
	} else {
		s.resultBuf = s.resultBuf[:len(all)]
	}
	for i, c := range all {
		s.resultBuf[i] = HNSWSearchResult{
			ID:    g.ids[c.nodeID],
			Value: g.nodeVector(c.nodeID),
			Score: c.dist,
		}
	}
	return s.resultBuf, nil
}

// ---------------------------------------------------------------------------
// StitchedSearch — PQ-accelerated graph navigation (DiskANN style)
//
// Uses PQ distances for graph traversal and exact (SIMD) distances for
// final reranking. The graph is navigated using compressed codes; only
// the top (2 * topK) candidates are reranked with exact distances.
// ---------------------------------------------------------------------------

func (g *VamanaGraph) StitchedSearch(
	query []float32,
	topK int,
	pqCodes [][]byte,
	pqTable [][]float32,
	distanceADC func(codes []byte) float32,
) ([]HNSWSearchResult, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if len(query) != g.dim {
		return nil, fmt.Errorf("dim mismatch: expected %d, got %d", g.dim, len(query))
	}
	if g.count == 0 {
		return nil, nil
	}

	s := g.scratchPool.Get().(*searchScratch)
	defer g.scratchPool.Put(s)
	s.queryNorm = float32(math.Sqrt(float64(simd.Dot(query, query))))

	if pqCodes == nil || distanceADC == nil {
		return g.searchWithScratch(query, topK, s)
	}

	// Phase 1: graph traversal with PQ-ADC distances
	ep := g.greedyDescendScratch(query, g.ep, s)

	result := &s.resultMax
	result.data = result.data[:0]
	candidates := &s.candHeap
	candidates.data = candidates.data[:0]

	epDist := distanceADC(pqCodes[ep])
	s.visitGen++
	g.visitScratch(s, ep)
	candidates.Push(searchCandidate{nodeID: ep, dist: epDist})
	result.Push(searchCandidate{nodeID: ep, dist: epDist})

	searchL := g.L
	if searchL < topK*2 {
		searchL = topK * 2
	}

	for candidates.Len() > 0 {
		c := candidates.Pop()
		worst := result.data[0].dist
		if c.dist > worst && result.Len() >= searchL {
			break
		}
		for _, nbrID := range g.neighbors[c.nodeID] {
			if g.isVisitedScratch(s, nbrID) {
				continue
			}
			g.visitScratch(s, nbrID)
			d := distanceADC(pqCodes[nbrID])
			if result.Len() >= searchL && d >= result.data[0].dist {
				continue
			}
			cand := searchCandidate{nodeID: nbrID, dist: d}
			candidates.Push(cand)
			result.Push(cand)
			if result.Len() > searchL {
				result.Pop()
			}
		}
	}

	// Phase 2: exact rerank of top candidates
	all := result.data[:min(searchL, len(result.data))]
	for i := 0; i < len(all); i++ {
		all[i].dist = g.distanceFromStoredScratch(all[i].nodeID, g.nodeVector(all[i].nodeID), query, s)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].dist < all[j].dist })

	topK = min(topK, len(all))
	all = all[:topK]

	if cap(s.resultBuf) < topK {
		s.resultBuf = make([]HNSWSearchResult, topK)
	} else {
		s.resultBuf = s.resultBuf[:topK]
	}
	for i, c := range all {
		s.resultBuf[i] = HNSWSearchResult{
			ID:    g.ids[c.nodeID],
			Value: g.nodeVector(c.nodeID),
			Score: c.dist,
		}
	}
	return s.resultBuf, nil
}

// ---------------------------------------------------------------------------
// Serialization
// ---------------------------------------------------------------------------

const _vamanaHeader = 28

func (g *VamanaGraph) neighborTotal() int {
	n := 0
	for i := 0; i < g.count && i < len(g.neighbors); i++ {
		if g.neighbors[i] != nil {
			n += len(g.neighbors[i])
		}
	}
	return n
}

func (g *VamanaGraph) FileSize() int {
	return _vamanaHeader +
		g.count*8 + // ids
		g.count*g.dim*4 + // vectors
		(g.count+1)*4 + // neighbor offsets
		g.neighborTotal()*4 // neighbor data
}

func (g *VamanaGraph) flattenTo(data []byte) {
	off := 0
	put32 := func(v uint32) {
		binary.LittleEndian.PutUint32(data[off:], v)
		off += 4
	}
	put64 := func(v uint64) {
		binary.LittleEndian.PutUint64(data[off:], v)
		off += 8
	}

	put32(uint32(g.count))
	put32(uint32(g.dim))
	put32(uint32(g.R))
	put32(uint32(g.L))
	put64(math.Float64bits(g.alpha))
	put32(uint32(g.ep))

	for i := 0; i < g.count; i++ {
		put64(g.ids[i])
	}
	for i := 0; i < g.count*g.dim; i++ {
		put32(math.Float32bits(g.vectors[i]))
	}

	offOff := off
	off += (g.count + 1) * 4

	for i := 0; i < g.count; i++ {
		start := uint32((off - _vamanaHeader - g.count*8 - g.count*g.dim*4 - (g.count+1)*4) / 4)
		binary.LittleEndian.PutUint32(data[offOff+i*4:], start)
		if g.neighbors != nil && i < len(g.neighbors) && g.neighbors[i] != nil {
			for _, v := range g.neighbors[i] {
				put32(uint32(v))
			}
		}
	}
	binary.LittleEndian.PutUint32(data[offOff+g.count*4:],
		uint32((off-_vamanaHeader-g.count*8-g.count*g.dim*4-(g.count+1)*4)/4))
}

func (g *VamanaGraph) WriteFile(path string) error {
	g.mu.RLock()
	data := make([]byte, g.FileSize())
	g.flattenTo(data)
	g.mu.RUnlock()
	return os.WriteFile(path, data, 0644)
}

func (g *VamanaGraph) Serialize() ([]byte, error) {
	g.mu.RLock()
	data := make([]byte, g.FileSize())
	g.flattenTo(data)
	g.mu.RUnlock()
	return data, nil
}

func (g *VamanaGraph) Deserialize(data []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(data) < _vamanaHeader {
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
	g.dim = int(read32())
	g.R = int(read32())
	g.L = int(read32())
	g.alpha = math.Float64frombits(read64())
	g.ep = int32(read32())
	g.count = count
	g.capacity = count

	g.ids = make([]uint64, count)
	for i := 0; i < count; i++ {
		g.ids[i] = read64()
	}

	g.vectors = make([]float32, count*g.dim)
	for i := range g.vectors {
		g.vectors[i] = math.Float32frombits(read32())
	}

	g.neighbors = make([][]int32, count)
	for i := 0; i < count; i++ {
		start := binary.LittleEndian.Uint32(data[off+i*4:])
		end := binary.LittleEndian.Uint32(data[off+(i+1)*4:])
		nlen := int(end - start)
		if nlen > 0 {
			nbrs := make([]int32, nlen)
			doff := _vamanaHeader + count*8 + count*g.dim*4 + (count+1)*4 + int(start)*4
			for j := range nbrs {
				nbrs[j] = int32(binary.LittleEndian.Uint32(data[doff+j*4:]))
			}
			g.neighbors[i] = nbrs
		}
	}

	g.norms = make([]float32, g.count)
	for i := 0; i < g.count; i++ {
		vec := g.nodeVector(int32(i))
		g.norms[i] = float32(math.Sqrt(float64(simd.Dot(vec, vec))))
	}

	bufSize := g.L
	if bufSize < 64 {
		bufSize = 64
	}
	g.candHeap = minHeap{data: make([]searchCandidate, 0, bufSize)}
	g.resultMax = maxHeap{data: make([]searchCandidate, 0, bufSize)}
	g.visited = make([]int32, count)
	g.scratchPool = sync.Pool{New: func() any {
		return &searchScratch{
			candHeap:  minHeap{data: make([]searchCandidate, 0, bufSize)},
			resultMax: maxHeap{data: make([]searchCandidate, 0, bufSize)},
			visited:   make([]int32, count),
			resultBuf: make([]HNSWSearchResult, 0, bufSize),
		}
	}}
	return nil
}

func (g *VamanaGraph) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mmapReader != nil {
		return g.mmapReader.Close()
	}
	return nil
}
