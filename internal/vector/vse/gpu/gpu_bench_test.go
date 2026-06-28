//go:build cuda

package gpu

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"nexus/internal/vector/vse"
	"nexus/internal/vector/vse/engine/quantizer"
)

// ---------------------------------------------------------------------------
// Synthetic data
// ---------------------------------------------------------------------------

type benchData struct {
	nv, dim, M, subdim, nc int
	vecs          []float32
	gids          []uint64
	pq            *quantizer.PQQuantizer
	ivf           *quantizer.IVF
	meta          *vse.SegmentMeta
	vectorsBin    []byte
}

func makeBenchData(nv, dim int, rng *rand.Rand) *benchData {
	if dim < 4 { dim = 4 }
	subVecs := dim / 4
	if subVecs < 1 { subVecs = 1 }
	subdim := dim / subVecs

	nc := 256

	vecs := make([]float32, nv*dim)
	for i := range vecs {
		vecs[i] = rng.Float32()*2 - 1
	}

	gids := make([]uint64, nv)
	for i := range gids {
		gids[i] = uint64(i + 1)
	}

	// Build PQ (quantizer with random centroids, but already trained with centroids set)
	pq := quantizer.NewPQQuantizer(dim, subVecs, 8)
	// Manually train by setting centroids directly
	// Generate random centroids [subVecs][256][subdim]
	cents3d := make([][][]float32, subVecs)
	for s := 0; s < subVecs; s++ {
		cents3d[s] = make([][]float32, 256)
		for k := 0; k < 256; k++ {
			cents3d[s][k] = make([]float32, subdim)
			for i := 0; i < subdim; i++ {
				cents3d[s][k][i] = rng.Float32()*2 - 1
			}
		}
	}

	// We'll directly encode vectors using our own PQ logic, but the pinSegment
	// in gpu_cuda.go calls pq.Centroids() and pq.SubVecs(), pq.SubDim().
	// So we need a PQQuantizer that has its centroids set.
	// The issue is centroids is unexported. Let me check...

	// Actually, looking at gpu_cuda.go PinSegment:
	//   pq_codebook := flattenCentroids(pq.Centroids())
	// We need pq.Centroids() to work. But Centroids() returns [][][]float32.
	// The PQ has centroids set to empty slices on creation. But we can't access them directly.
	// Let me train with a subset of vectors.

	// Train PQ on a subset of vectors
	trainVecs := make([][]float32, nv)
	for i := 0; i < nv; i++ {
		trainVecs[i] = vecs[i*dim : (i+1)*dim]
	}
	if err := pq.Train(trainVecs); err != nil {
		panic(fmt.Sprintf("PQ train: %v", err))
	}

	// Train IVF
	ivf := quantizer.NewIVF(dim, nc)
	if err := ivf.Train(trainVecs); err != nil {
		panic(fmt.Sprintf("IVF train: %v", err))
	}

	// Encode PQ codes
	pqCodes := make([]byte, nv*subVecs)
	for v := 0; v < nv; v++ {
		codes, err := pq.Encode(vecs[v*dim : (v+1)*dim])
		if err != nil {
			panic(fmt.Sprintf("PQ encode: %v", err))
		}
		copy(pqCodes[v*subVecs:], codes)
	}

	// Build vectors.bin: [4:offset][4:nv][8+4*dim per entry...]
	rowSize := 8 + dim*4
	nvOffset := 8
	binSize := nvOffset + nv*rowSize
	bin := make([]byte, binSize)
	binary.LittleEndian.PutUint32(bin[0:4], uint32(nvOffset)) // offset to first entry
	binary.LittleEndian.PutUint32(bin[4:8], uint32(nv))       // num vectors
	for i := 0; i < nv; i++ {
		off := nvOffset + i*rowSize
		binary.LittleEndian.PutUint64(bin[off:off+8], gids[i])
		for j := 0; j < dim; j++ {
			binary.LittleEndian.PutUint32(bin[off+8+j*4:off+12+j*4], math.Float32bits(vecs[i*dim+j]))
		}
	}

	meta := &vse.SegmentMeta{
		ID:         42,
		NumVectors: nv,
		Dimension:  dim,
		Tier:       vse.TierCold,
	}

	return &benchData{
		nv: nv, dim: dim, M: subVecs, subdim: subdim, nc: nc,
		vecs: vecs, gids: gids,
		pq: pq, ivf: ivf,
		meta: meta, vectorsBin: bin,
	}
}

// CPU exact search reference
func cpuFlatSearch(bd *benchData, query []float32, topK int) []vse.SearchResult {
	type pair struct {
		dist float32
		idx  int
	}
	pairs := make([]pair, bd.nv)
	for i := 0; i < bd.nv; i++ {
		s := float32(0)
		for j := 0; j < bd.dim; j++ {
			d := query[j] - bd.vecs[i*bd.dim+j]
			s += d * d
		}
		pairs[i] = pair{dist: s, idx: i}
	}
	for i := 0; i < topK && i < len(pairs); i++ {
		best := i
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].dist < pairs[best].dist {
				best = j
			}
		}
		pairs[i], pairs[best] = pairs[best], pairs[i]
	}
	if topK > bd.nv { topK = bd.nv }
	res := make([]vse.SearchResult, topK)
	for i := 0; i < topK; i++ {
		res[i] = vse.SearchResult{
			ID:        vse.VectorID(bd.gids[pairs[i].idx]),
			Score:     pairs[i].dist,
			SegmentID: 42,
		}
	}
	return res
}

// CPU approximate PQ search reference
func cpuPQSearch(bd *benchData, query []float32, topK int) []vse.SearchResult {
	table := bd.pq.PrecomputeQueryDistances(query)
	type pair struct {
		dist float32
		idx  int
	}
	pairs := make([]pair, bd.nv)
	for i := 0; i < bd.nv; i++ {
		codes, _ := bd.pq.Encode(bd.vecs[i*bd.dim : (i+1)*bd.dim])
		d := bd.pq.DistanceADC(table, codes)
		pairs[i] = pair{dist: d, idx: i}
	}
	for i := 0; i < topK && i < len(pairs); i++ {
		best := i
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].dist < pairs[best].dist {
				best = j
			}
		}
		pairs[i], pairs[best] = pairs[best], pairs[i]
	}
	if topK > bd.nv { topK = bd.nv }
	res := make([]vse.SearchResult, topK)
	for i := 0; i < topK; i++ {
		res[i] = vse.SearchResult{
			ID:        vse.VectorID(bd.gids[pairs[i].idx]),
			Score:     pairs[i].dist,
			SegmentID: 42,
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkGPU(b *testing.B) {
	rng := rand.New(rand.NewSource(42))

	// Single GPU manager for all bench cases
	mgr := NewManager(DefaultConfig())
	if err := mgr.Init(); err != nil {
		b.Skipf("GPU init failed: %v", err)
	}
	defer mgr.Close()

	benches := []struct {
		name string
		nv   int
		dim  int
	}{
		{"10Kx128d", 10_000, 128},
		{"10Kx384d", 10_000, 384},
		{"50Kx128d", 50_000, 128},
		{"50Kx384d", 50_000, 384},
		{"100Kx128d", 100_000, 128},
		{"100Kx384d", 100_000, 384},
		{"500Kx128d", 500_000, 128},
		{"500Kx384d", 500_000, 384},
	}

	for _, bc := range benches {
		if bc.nv*bc.dim*4 > 3_000_000_000 {
			continue
		}
		bd := makeBenchData(bc.nv, bc.dim, rng)

		// Use unique segment ID per bench case
		segID := vse.SegmentID(bc.nv + bc.dim)
		bd.meta.ID = segID

		if err := mgr.PinSegment(bd.meta, bd.vectorsBin, bd.pq, bd.ivf); err != nil {
			b.Fatalf("PinSegment: %v", err)
		}

		query := make([]float32, bc.dim)
		for i := range query {
			query[i] = rng.Float32()*2 - 1
		}

		// --- GPU flat exact (single query) ---
		b.Run("GPU_flat/"+bc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, err := mgr.Search(segID, SearchRequest{
					Query:  query,
					TopK:   10,
					Exact:  true,
					Metric: vse.MetricEuclidean,
				})
				if err != nil {
					b.Fatal(err)
				}
				_ = r
			}
		})

		// --- GPU PQ approximate (single query) ---
		b.Run("GPU_PQ/"+bc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, err := mgr.Search(segID, SearchRequest{
					Query:  query,
					TopK:   10,
					NProbe: 64,
					Metric: vse.MetricEuclidean,
				})
				if err != nil {
					b.Fatal(err)
				}
				_ = r
			}
		})

		// --- GPU batch (16 queries) ---
		batchSize := 16
		b.Run("GPU_batch16/"+bc.name, func(b *testing.B) {
			reqs := make([]SearchRequest, batchSize)
			for i := range reqs {
				q := make([]float32, bc.dim)
				for j := range q {
					q[j] = rng.Float32()*2 - 1
				}
				reqs[i] = SearchRequest{Query: q, TopK: 10, NProbe: 64, Metric: vse.MetricEuclidean}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = mgr.BatchSearch(segID, reqs)
			}
		})

		// --- GPU batch (64 queries) ---
		batchSize64 := 64
		b.Run("GPU_batch64/"+bc.name, func(b *testing.B) {
			reqs := make([]SearchRequest, batchSize64)
			for i := range reqs {
				q := make([]float32, bc.dim)
				for j := range q {
					q[j] = rng.Float32()*2 - 1
				}
				reqs[i] = SearchRequest{Query: q, TopK: 10, NProbe: 64, Metric: vse.MetricEuclidean}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = mgr.BatchSearch(segID, reqs)
			}
		})

		// --- CPU exact flat reference ---
		b.Run("CPU_flat/"+bc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = cpuFlatSearch(bd, query, 10)
			}
		})

		// --- CPU PQ reference ---
		b.Run("CPU_PQ/"+bc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = cpuPQSearch(bd, query, 10)
			}
		})

		mgr.UnpinSegment(segID)
	}
}
