//go:build cuda

package gpu

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"

	"cipherlake/internal/vector/vse"
	"cipherlake/internal/vector/vse/engine/quantizer"
)

// ---------------------------------------------------------------------------
// fvecs / ivecs loader
// ---------------------------------------------------------------------------

func loadFvecs(path string) ([]float32, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(data) < 4 {
		return nil, 0, fmt.Errorf("file too small: %d bytes", len(data))
	}
	dim := int(binary.LittleEndian.Uint32(data[:4]))
	vecSize := 4 + dim*4
	n := len(data) / vecSize
	if n*vecSize != len(data) {
		return nil, 0, fmt.Errorf("file size %d not aligned to vec size %d", len(data), vecSize)
	}
	vecs := make([]float32, n*dim)
	for i := 0; i < n; i++ {
		off := i*vecSize + 4
		for j := 0; j < dim; j++ {
			vecs[i*dim+j] = math.Float32frombits(binary.LittleEndian.Uint32(data[off+j*4:]))
		}
	}
	return vecs, dim, nil
}

func loadIvecs(path string) ([][]int32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, fmt.Errorf("file too small: %d bytes", len(data))
	}
	dim := int(binary.LittleEndian.Uint32(data[:4]))
	vecSize := 4 + dim*4
	n := len(data) / vecSize
	if n*vecSize != len(data) {
		return nil, fmt.Errorf("file size %d not aligned to vec size %d", len(data), vecSize)
	}
	result := make([][]int32, n)
	for i := 0; i < n; i++ {
		off := i*vecSize + 4
		row := make([]int32, dim)
		for j := 0; j < dim; j++ {
			row[j] = int32(binary.LittleEndian.Uint32(data[off+j*4:]))
		}
		result[i] = row
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Build vectors.bin from flat vectors
// ---------------------------------------------------------------------------

func buildVectorsBin(vecs []float32, dim, nv int) []byte {
	rowSize := 8 + dim*4
	bin := make([]byte, 8+nv*rowSize)
	binary.LittleEndian.PutUint64(bin[0:8], uint64(nv))
	for i := 0; i < nv; i++ {
		off := 8 + i*rowSize
		binary.LittleEndian.PutUint64(bin[off:off+8], uint64(i+1))
		for j := 0; j < dim; j++ {
			binary.LittleEndian.PutUint32(bin[off+8+j*4:off+12+j*4], math.Float32bits(vecs[i*dim+j]))
		}
	}
	return bin
}

// ---------------------------------------------------------------------------
// SIFT1M test
// ---------------------------------------------------------------------------

// From internal/vector/vse/gpu/, go up 4 levels to project root
const dataDir = "../../../../data"

func TestSIFT1MRecall(t *testing.T) {
	if testing.Short() {
		t.Skip("SIFT1M GPU benchmark-style test skipped in short mode")
	}
	// ---- Step 1: Load training set ----
	t.Log("Loading sift_learn.fvecs...")
	learnVecs, dim, err := loadFvecs(dataDir + "/sift_learn.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_learn.fvecs: %v", err)
	}
	learnNv := len(learnVecs) / dim
	t.Logf("  %d vectors, dim=%d", learnNv, dim)

	// ---- Step 2: Train PQ + IVF ----
	t.Log("Training PQ (M=32, 8-bit)...")
	pq := quantizer.NewPQQuantizer(dim, 32, 8)
	learnSlice := make([][]float32, learnNv)
	for i := 0; i < learnNv; i++ {
		learnSlice[i] = learnVecs[i*dim : (i+1)*dim]
	}
	if err := pq.Train(learnSlice); err != nil {
		t.Fatalf("PQ train: %v", err)
	}
	t.Logf("  M=%d subdim=%d", pq.SubVecs(), pq.SubDim())

	t.Log("Training IVF (nc=256)...")
	ivf := quantizer.NewIVF(dim, 256)
	if err := ivf.Train(learnSlice); err != nil {
		t.Fatalf("IVF train: %v", err)
	}
	t.Log("  done")

	// ---- Step 3: Load base set ----
	t.Log("Loading sift_base.fvecs (1M)...")
	baseVecsFull, _, err := loadFvecs(dataDir + "/sift_base.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_base.fvecs: %v", err)
	}
	baseNv := 1_000_000
	dimB := dim
	baseVecs := baseVecsFull
	vectorsBin := buildVectorsBin(baseVecs, dimB, baseNv)

	// ---- Step 4: Load queries ----
	t.Log("Loading sift_query.fvecs...")
	queryVecs, _, err := loadFvecs(dataDir + "/sift_query.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_query.fvecs: %v", err)
	}
	nq := 100 // first 100 queries (full 10K would take too long)
	queryVecs = queryVecs[:nq*dim]

	// ---- Step 5: Load ground truth ----
	t.Log("Loading sift_groundtruth.ivecs...")
	gt, err := loadIvecs(dataDir + "/sift_groundtruth.ivecs")
	if err != nil {
		t.Skipf("skip: cannot load groundtruth: %v", err)
	}
	gt = gt[:nq]

	// ---- Step 6: GPU init ----
	t.Log("Initializing GPU...")
	mgr := NewManager(Config{
		RerankCandidates: 10000,
		NProbe:           16,
		PTXPath:          "kernels.ptx",
	})
	if err := mgr.Init(); err != nil {
		t.Fatalf("GPU init: %v", err)
	}
	defer mgr.Close()

	segID := vse.SegmentID(42)
	meta := &vse.SegmentMeta{
		ID:         segID,
		NumVectors: baseNv,
		Dimension:  dimB,
		Tier:       vse.TierCold,
	}

	// 将 base 集分配到 IVF lists（与 CPU IVF-PQ 测试一致）
	t.Log("Assigning base vectors to IVF lists...")
	for i := 0; i < baseNv; i++ {
		vec := baseVecs[i*dim : (i+1)*dim]
		bestD := float32(math.MaxFloat32)
		bestK := 0
		for j, c := range ivf.Centroids {
			d := float32(0)
			for di := 0; di < dim; di++ {
				diff := vec[di] - c[di]
				d += diff * diff
			}
			if d < bestD {
				bestD = d
				bestK = j
			}
		}
		ivf.Lists[bestK] = append(ivf.Lists[bestK], i)
	}
	t.Logf("  IVF assigned, nprobe=16, rerank=10000")

	if err := mgr.PinSegment(meta, vectorsBin, pq, ivf); err != nil {
		t.Fatalf("PinSegment: %v", err)
	}
	defer mgr.UnpinSegment(segID)

	// ---- Step 7: Recall test ----
	t.Log("Running recall test...")
	recalled10 := 0
	recalled100 := 0
	total10 := 0
	total100 := 0

	for i := 0; i < nq; i++ {
		query := queryVecs[i*dim : (i+1)*dim]
		res, err := mgr.Search(segID, SearchRequest{
			Query:  query,
			TopK:   100,
			NProbe: 16,
			Metric: vse.MetricEuclidean,
		})
		if err != nil {
			t.Fatalf("search query %d: %v", i, err)
		}

		hit := make(map[uint64]bool)
		for _, r := range res {
			hit[uint64(r.ID)] = true
		}

		for k := 0; k < 100; k++ {
			// Ground truth indices are 0-based into the full 1M base.
			gtIdx := int(gt[i][k])
			if k < 10 {
				total10++
			}
			total100++
			gid := uint64(gtIdx + 1) // our gids start at 1
			if hit[gid] {
				if k < 10 {
					recalled10++
				}
				recalled100++
			}
		}

		if (i+1)%200 == 0 {
			t.Logf("  %d/%d queries done", i+1, nq)
		}
	}

	recall10 := float64(recalled10) / float64(total10)
	recall100 := float64(recalled100) / float64(total100)
	t.Logf("Recall@10: %.2f%% (%d/%d)", recall10*100, recalled10, total10)
	t.Logf("Recall@100: %.2f%% (%d/%d)", recall100*100, recalled100, total100)
}

// ---------------------------------------------------------------------------
// Benchmark: GPU vs CPU on 100K SIFT1M
// ---------------------------------------------------------------------------

func BenchmarkSIFT1M_100K(b *testing.B) {
	// ---- Load + train ----
	learnVecs, dim, err := loadFvecs(dataDir + "/sift_learn.fvecs")
	if err != nil {
		b.Skipf("skip: %v", err)
	}
	learnNv := len(learnVecs) / dim

	pq := quantizer.NewPQQuantizer(dim, 32, 8)
	learnSlice := make([][]float32, learnNv)
	for i := 0; i < learnNv; i++ {
		learnSlice[i] = learnVecs[i*dim : (i+1)*dim]
	}
	if err := pq.Train(learnSlice); err != nil {
		b.Fatalf("PQ train: %v", err)
	}
	ivf := quantizer.NewIVF(dim, 256)
	if err := ivf.Train(learnSlice); err != nil {
		b.Fatalf("IVF train: %v", err)
	}

	// ---- Load 100K base ----
	baseVecsFull, _, err := loadFvecs(dataDir + "/sift_base.fvecs")
	if err != nil {
		b.Skipf("skip: %v", err)
	}
	baseNv := 100_000
	baseVecs := baseVecsFull[:baseNv*dim]
	vectorsBin := buildVectorsBin(baseVecs, dim, baseNv)

	// ---- Load queries ----
	queryVecs, _, err := loadFvecs(dataDir + "/sift_query.fvecs")
	if err != nil {
		b.Skipf("skip: %v", err)
	}
	nq := 100
	queryVecs = queryVecs[:nq*dim]

	// ---- GPU init ----
	mgr := NewManager(Config{
		RerankCandidates: 5000,
		NProbe:           64,
		PTXPath:          "kernels.ptx",
	})
	if err := mgr.Init(); err != nil {
		b.Fatalf("GPU init: %v", err)
	}
	defer mgr.Close()

	segID := vse.SegmentID(77)
	meta := &vse.SegmentMeta{
		ID:         segID,
		NumVectors: baseNv,
		Dimension:  dim,
		Tier:       vse.TierCold,
	}
	// 将 base 集分配到 IVF lists（与 recall 测试一致）
	for i := 0; i < baseNv; i++ {
		vec := baseVecs[i*dim : (i+1)*dim]
		bestD := float32(math.MaxFloat32)
		bestK := 0
		for j, c := range ivf.Centroids {
			d := float32(0)
			for di := 0; di < dim; di++ {
				diff := vec[di] - c[di]
				d += diff * diff
			}
			if d < bestD {
				bestD = d
				bestK = j
			}
		}
		ivf.Lists[bestK] = append(ivf.Lists[bestK], i)
	}

	if err := mgr.PinSegment(meta, vectorsBin, pq, ivf); err != nil {
		b.Fatalf("PinSegment: %v", err)
	}
	defer mgr.UnpinSegment(segID)

	// Pre-encode PQ codes for CPU PQ benchmark
	pqCodes := make([][]byte, baseNv)
	for i := 0; i < baseNv; i++ {
		code, _ := pq.Encode(baseVecs[i*dim : (i+1)*dim])
		pqCodes[i] = code
	}

	b.ResetTimer()

	// ---- GPU PQ single query ----
	b.Run("GPU_PQ_single", func(b *testing.B) {
		query := queryVecs[0*dim : 1*dim]
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := mgr.Search(segID, SearchRequest{
				Query:  query,
				TopK:   10,
				NProbe: 64,
				Metric: vse.MetricEuclidean,
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	// ---- GPU flat single query ----
	b.Run("GPU_flat_single", func(b *testing.B) {
		query := queryVecs[0*dim : 1*dim]
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := mgr.Search(segID, SearchRequest{
				Query:  query,
				TopK:   10,
				Exact:  true,
				Metric: vse.MetricEuclidean,
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	// ---- CPU flat single query ----
	b.Run("CPU_flat_single", func(b *testing.B) {
		query := queryVecs[0*dim : 1*dim]
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cpuFlatSearchSIFT(baseVecs, dim, baseNv, query, 10)
		}
	})

	// ---- CPU PQ single query ----
	b.Run("CPU_PQ_single", func(b *testing.B) {
		query := queryVecs[0*dim : 1*dim]
		table := pq.PrecomputeQueryDistances(query)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cpuPQSearchSIFT(table, pqCodes, query, baseVecs, dim, baseNv, 10)
		}
	})
}

// ---- CPU helpers ----

func cpuFlatSearchSIFT(vecs []float32, dim, nv int, query []float32, topK int) []vse.SearchResult {
	type pair struct {
		dist float32
		idx  int
	}
	pairs := make([]pair, nv)
	for i := 0; i < nv; i++ {
		s := float32(0)
		for j := 0; j < dim; j++ {
			d := query[j] - vecs[i*dim+j]
			s += d * d
		}
		pairs[i] = pair{dist: s, idx: i}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].dist < pairs[j].dist })
	if topK > nv {
		topK = nv
	}
	res := make([]vse.SearchResult, topK)
	for i := 0; i < topK; i++ {
		res[i] = vse.SearchResult{
			ID:    vse.VectorID(pairs[i].idx + 1),
			Score: pairs[i].dist,
		}
	}
	return res
}

func assignToIVF(ivf *quantizer.IVF, vecs []float32, dim, nv int) [][]int {
	lists := make([][]int, ivf.Ncentroids)
	reportEvery := nv / 10
	if reportEvery < 1 {
		reportEvery = 1
	}
	for i := 0; i < nv; i++ {
		if i%reportEvery == 0 {
			fmt.Printf("    IVF assign: %d/%d (%.0f%%)\n", i, nv, float64(i)*100/float64(nv))
		}
		vec := vecs[i*dim : (i+1)*dim]
		bestD := float32(math.MaxFloat32)
		bestK := 0
		for j, c := range ivf.Centroids {
			d := float32(0)
			for di := 0; di < dim; di++ {
				diff := vec[di] - c[di]
				d += diff * diff
			}
			if d < bestD {
				bestD = d
				bestK = j
			}
		}
		lists[bestK] = append(lists[bestK], i)
	}
	fmt.Printf("    IVF assign: %d/%d (100%%)\n", nv, nv)
	return lists
}

func cpuPQSearchSIFT(table [][]float32, pqCodes [][]byte, query []float32, vecs []float32, dim, nv, topK int) []vse.SearchResult {
	type pair struct {
		dist float32
		idx  int
	}
	pairs := make([]pair, nv)
	for i := 0; i < nv; i++ {
		d := float32(0)
		for s := 0; s < len(table); s++ {
			if int(pqCodes[i][s]) < len(table[s]) {
				d += table[s][pqCodes[i][s]]
			}
		}
		pairs[i] = pair{dist: d, idx: i}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].dist < pairs[j].dist })
	if topK > nv {
		topK = nv
	}
	res := make([]vse.SearchResult, topK)
	for i := 0; i < topK; i++ {
		res[i] = vse.SearchResult{
			ID:    vse.VectorID(pairs[i].idx + 1),
			Score: pairs[i].dist,
		}
	}
	return res
}

// BenchmarkSIFT1M_1M 测试 1M 向量规模下 GPU vs CPU 的性能差异
func BenchmarkSIFT1M_1M(b *testing.B) {
	dataDir := "../../../../data"

	// Load learn set for training
	learnVecs, dimLearn, err := loadFvecs(dataDir + "/sift_learn.fvecs")
	if err != nil {
		b.Skipf("skip: %v", err)
	}
	learnNv := len(learnVecs) / dimLearn

	// Train PQ
	pq := quantizer.NewPQQuantizer(dimLearn, 32, 8)
	learnSlice := make([][]float32, learnNv)
	for i := 0; i < learnNv; i++ {
		learnSlice[i] = learnVecs[i*dimLearn : (i+1)*dimLearn]
	}
	if err := pq.Train(learnSlice); err != nil {
		b.Fatalf("PQ train: %v", err)
	}

	// Train IVF
	ivf := quantizer.NewIVF(dimLearn, 256)
	if err := ivf.Train(learnSlice); err != nil {
		b.Fatalf("IVF train: %v", err)
	}

	// Load 1M base set
	baseVecs, dim, err := loadFvecs(dataDir + "/sift_base.fvecs")
	if err != nil {
		b.Skipf("skip: %v", err)
	}
	baseNv := len(baseVecs) / dim
	vectorsBin := buildVectorsBin(baseVecs, dim, baseNv)

	// Load queries
	queryVecs, _, err := loadFvecs(dataDir + "/sift_query.fvecs")
	if err != nil {
		b.Skipf("skip: %v", err)
	}
	nq := 100
	queryVecs = queryVecs[:nq*dim]

	// GPU init
	mgr := NewManager(Config{
		RerankCandidates: 10000,
		NProbe:           16,
		PTXPath:          "kernels.ptx",
	})
	if err := mgr.Init(); err != nil {
		b.Fatalf("GPU init: %v", err)
	}
	defer mgr.Close()

	segID := vse.SegmentID(88)
	meta := &vse.SegmentMeta{
		ID:         segID,
		NumVectors: baseNv,
		Dimension:  dim,
		Tier:       vse.TierCold,
	}

	// Assign base vectors to IVF lists
	for i := 0; i < baseNv; i++ {
		vec := baseVecs[i*dim : (i+1)*dim]
		bestD := float32(math.MaxFloat32)
		bestK := 0
		for j, c := range ivf.Centroids {
			d := float32(0)
			for di := 0; di < dim; di++ {
				diff := vec[di] - c[di]
				d += diff * diff
			}
			if d < bestD {
				bestD = d
				bestK = j
			}
		}
		ivf.Lists[bestK] = append(ivf.Lists[bestK], i)
	}

	if err := mgr.PinSegment(meta, vectorsBin, pq, ivf); err != nil {
		b.Fatalf("PinSegment: %v", err)
	}
	defer mgr.UnpinSegment(segID)

	// Pre-encode PQ codes for CPU PQ benchmark
	pqCodes := make([][]byte, baseNv)
	for i := 0; i < baseNv; i++ {
		code, _ := pq.Encode(baseVecs[i*dim : (i+1)*dim])
		pqCodes[i] = code
	}

	b.ResetTimer()

	// GPU PQ single query (with rerank)
	b.Run("GPU_PQ_rerank", func(b *testing.B) {
		query := queryVecs[0*dim : 1*dim]
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := mgr.Search(segID, SearchRequest{
				Query:  query,
				TopK:   10,
				NProbe: 16,
				Metric: vse.MetricEuclidean,
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	// GPU flat single query (exact)
	b.Run("GPU_flat_exact", func(b *testing.B) {
		query := queryVecs[0*dim : 1*dim]
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := mgr.Search(segID, SearchRequest{
				Query:  query,
				TopK:   10,
				Exact:  true,
				Metric: vse.MetricEuclidean,
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	// CPU flat single query
	b.Run("CPU_flat", func(b *testing.B) {
		query := queryVecs[0*dim : 1*dim]
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cpuFlatSearchSIFT(baseVecs, dim, baseNv, query, 10)
		}
	})

	// CPU PQ single query
	b.Run("CPU_PQ", func(b *testing.B) {
		query := queryVecs[0*dim : 1*dim]
		table := pq.PrecomputeQueryDistances(query)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cpuPQSearchSIFT(table, pqCodes, query, baseVecs, dim, baseNv, 10)
		}
	})
}
