package index

import "fmt"

type HNSWIndex struct {
	graph *FlatHNSW
	dim   int
	nodes int
}

func NewHNSWIndex(dim int, m int, efSearch int, ml float64, df func(a, b []float32) float32) *HNSWIndex {
	mt := detectMetric(df)
	graph := NewFlatHNSW(dim, m, efSearch, ml, mt)
	return &HNSWIndex{
		graph: graph,
		dim:   dim,
	}
}

func WrapFlatHNSW(g *FlatHNSW) *HNSWIndex {
	return &HNSWIndex{
		graph: g,
		dim:   g.Dim(),
		nodes: g.Len(),
	}
}

func (idx *HNSWIndex) Insert(id uint64, vec []float32) error {
	if len(vec) != idx.dim {
		return fmt.Errorf("dimension mismatch: expected %d, got %d", idx.dim, len(vec))
	}
	if err := idx.graph.Insert(id, vec); err != nil {
		return err
	}
	idx.nodes++
	return nil
}

func (idx *HNSWIndex) InsertBatch(ids []uint64, vecs [][]float32) error {
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

type HNSWSearchResult struct {
	ID    uint64
	Value []float32
	Score float32
}

type HNSWSearchStats struct {
	EntryDescendNs   int64
	LayerSearchNs    int64
	ResultFinalizeNs int64
	DistanceCalls    int64
	CandidatePops    int64
	NeighborVisits   int64
	VisitedResets    int64
	VectorPrefetches int64
	SelectorPushes   int64
}

func (idx *HNSWIndex) Search(query []float32, topK int) ([]HNSWSearchResult, error) {
	if len(query) != idx.dim {
		return nil, fmt.Errorf("dimension mismatch: expected %d, got %d", idx.dim, len(query))
	}
	return idx.graph.Search(query, topK)
}

func (idx *HNSWIndex) SearchWithStats(query []float32, topK int) ([]HNSWSearchResult, HNSWSearchStats, error) {
	if len(query) != idx.dim {
		return nil, HNSWSearchStats{}, fmt.Errorf("dimension mismatch: expected %d, got %d", idx.dim, len(query))
	}
	return idx.graph.SearchWithStats(query, topK)
}

func (idx *HNSWIndex) Close() error {
	return idx.graph.Close()
}

func (idx *HNSWIndex) Delete(id uint64) bool {
	return false
}

func (idx *HNSWIndex) Len() int {
	return idx.graph.Len()
}

func (idx *HNSWIndex) WriteFile(path string) error {
	return idx.graph.WriteFile(path)
}

func (idx *HNSWIndex) Serialize() ([]byte, error) {
	return idx.graph.Serialize()
}

func (idx *HNSWIndex) Deserialize(data []byte, dim int, df func(a, b []float32) float32) error {
	graph := NewFlatHNSWFromFunc(dim, idx.graph.M, idx.graph.EfSearch, idx.graph.Ml, df)
	if err := graph.Deserialize(data); err != nil {
		return fmt.Errorf("hnsw deserialize: %w", err)
	}

	idx.graph = graph
	idx.dim = dim
	idx.nodes = graph.Len()
	return nil
}
