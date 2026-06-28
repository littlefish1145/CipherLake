package index

import (
	"sort"
	"sync"

	"nexus/internal/vector/vse/engine/quantizer"
	"nexus/internal/vector/vse/simd"
)

type IVFPQIndex struct {
	ivf         *quantizer.IVF
	pq          *quantizer.PQQuantizer
	mu          sync.RWMutex
	dim         int
	nprobe      int
	ncentroids  int
	topKVecs    int
}

func NewIVFPQIndex(dim, ncentroids, nprobe, subVecs, nbits, topKVecs int) *IVFPQIndex {
	return &IVFPQIndex{
		ivf:        quantizer.NewIVF(dim, ncentroids),
		pq:         quantizer.NewPQQuantizer(dim, subVecs, nbits),
		dim:        dim,
		nprobe:     nprobe,
		ncentroids: ncentroids,
		topKVecs:   topKVecs,
	}
}

func (idx *IVFPQIndex) Train(vectors [][]float32) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if err := idx.ivf.Train(vectors); err != nil {
		return err
	}
	if err := idx.pq.Train(vectors); err != nil {
		return err
	}
	return nil
}

type PQScoredResult struct {
	Idx      int
	PQDist   float32
	FullDist float32
	GlobalID uint64
}

func (idx *IVFPQIndex) Search(query []float32, globalIDs []uint64, getVec func(int) []float32, topK int) ([]PQScoredResult, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	nprobe := idx.nprobe
	if nprobe <= 0 {
		nprobe = 16
	}
	type cd struct {
		idx  int
		dist float32
	}
	cdists := make([]cd, len(idx.ivf.Centroids))
	for j, c := range idx.ivf.Centroids {
		// SIMD 加速的 L2 平方距离
		cdists[j] = cd{idx: j, dist: simd.L2Sq(query, c)}
	}

	sort.Slice(cdists, func(i, j int) bool {
		return cdists[i].dist < cdists[j].dist
	})
	if nprobe > len(cdists) {
		nprobe = len(cdists)
	}

	type scored struct {
		localIdx int
		fullDist float32
	}
	var candidates []scored

	for _, cd := range cdists[:nprobe] {
		for _, localIdx := range idx.ivf.Lists[cd.idx] {
			if localIdx < len(globalIDs) && getVec != nil {
				vec := getVec(localIdx)
				if vec != nil {
					// SIMD 加速的 L2 平方距离
					d := simd.L2Sq(vec, query)
					candidates = append(candidates, scored{localIdx: localIdx, fullDist: d})
				}
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].fullDist < candidates[j].fullDist
	})
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}

	results := make([]PQScoredResult, len(candidates))
	for i, c := range candidates {
		gid := uint64(0)
		if c.localIdx < len(globalIDs) {
			gid = globalIDs[c.localIdx]
		}
		results[i] = PQScoredResult{
			Idx:      c.localIdx,
			FullDist: c.fullDist,
			GlobalID: gid,
		}
	}
	return results, nil
}

func (idx *IVFPQIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	total := 0
	for _, list := range idx.ivf.Lists {
		total += len(list)
	}
	return total
}

func (idx *IVFPQIndex) IVF() *quantizer.IVF { return idx.ivf }
func (idx *IVFPQIndex) PQ() *quantizer.PQQuantizer { return idx.pq }
