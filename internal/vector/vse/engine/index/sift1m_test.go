package index

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
	"time"

	"cipherlake/internal/vector/vse"
	"cipherlake/internal/vector/vse/engine/quantizer"
	"cipherlake/internal/vector/vse/simd"
)

// ---------------------------------------------------------------------------
// fvecs / ivecs loader（与 gpu/sift1m_test.go 一致）
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

// dataDir 从 internal/vector/vse/engine/index/ 上溯 5 级到项目根
const dataDir = "../../../../../data"

// ---------------------------------------------------------------------------
// 辅助：计算召回率
// ---------------------------------------------------------------------------

func computeRecall(results []uint64, gt []int32, k int) (hit, total int) {
	hitSet := make(map[uint64]bool, len(results))
	for _, r := range results {
		hitSet[r] = true
	}
	for i := 0; i < k && i < len(gt); i++ {
		total++
		// ground truth 是 0-based 索引，我们的 ID 从 1 开始
		gid := uint64(gt[i] + 1)
		if hitSet[gid] {
			hit++
		}
	}
	return
}

// ---------------------------------------------------------------------------
// TestSIFT1M_Flat: 暴力搜索基线（验证正确性 + 延迟基线）
// ---------------------------------------------------------------------------

func TestSIFT1M_Flat(t *testing.T) {
	if testing.Short() {
		t.Skip("SIFT1M benchmark-style test skipped in short mode")
	}
	simd.Init()

	baseVecs, dim, err := loadFvecs(dataDir + "/sift_base.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_base.fvecs: %v", err)
	}
	baseNv := len(baseVecs) / dim
	t.Logf("base: %d vectors, dim=%d", baseNv, dim)

	queryVecs, _, err := loadFvecs(dataDir + "/sift_query.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_query.fvecs: %v", err)
	}
	nq := 100
	queryVecs = queryVecs[:nq*dim]

	gt, err := loadIvecs(dataDir + "/sift_groundtruth.ivecs")
	if err != nil {
		t.Skipf("skip: cannot load groundtruth: %v", err)
	}
	gt = gt[:nq]

	recalled10 := 0
	total10 := 0
	var latencies []float64

	for i := 0; i < nq; i++ {
		query := queryVecs[i*dim : (i+1)*dim]
		start := time.Now()

		// 暴力 L2 搜索
		type pair struct {
			dist float32
			idx  int
		}
		pairs := make([]pair, baseNv)
		for j := 0; j < baseNv; j++ {
			d := simd.L2Sq(query, baseVecs[j*dim:(j+1)*dim])
			pairs[j] = pair{dist: d, idx: j}
		}
		sort.Slice(pairs, func(a, b int) bool { return pairs[a].dist < pairs[b].dist })

		lat := time.Since(start)
		latencies = append(latencies, float64(lat.Microseconds()))

		topK := 10
		resultIDs := make([]uint64, topK)
		for j := 0; j < topK && j < len(pairs); j++ {
			resultIDs[j] = uint64(pairs[j].idx + 1)
		}

		hit, total := computeRecall(resultIDs, gt[i], topK)
		recalled10 += hit
		total10 += total

		if (i+1)%20 == 0 {
			t.Logf("  flat %d/%d queries", i+1, nq)
		}
	}

	recall := float64(recalled10) / float64(total10)
	p50, p99 := percentileFloat(latencies, 50), percentileFloat(latencies, 99)
	t.Logf("=== Flat Search (brute force) ===")
	t.Logf("Recall@10: %.2f%% (%d/%d)", recall*100, recalled10, total10)
	t.Logf("Latency  P50: %.1fµs  P99: %.1fµs", p50, p99)
	t.Logf("Avg: %.1fµs", avg(latencies))
}

// ---------------------------------------------------------------------------
// TestSIFT1M_HNSW: HNSW 索引构建 + 搜索 + 召回率
// ---------------------------------------------------------------------------

func TestSIFT1M_HNSW(t *testing.T) {
	if testing.Short() {
		t.Skip("SIFT1M benchmark-style test skipped in short mode")
	}
	simd.Init()

	baseVecs, dim, err := loadFvecs(dataDir + "/sift_base.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_base.fvecs: %v", err)
	}
	baseNv := len(baseVecs) / dim
	t.Logf("base: %d vectors, dim=%d", baseNv, dim)

	queryVecs, _, err := loadFvecs(dataDir + "/sift_query.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_query.fvecs: %v", err)
	}
	nq := 100
	queryVecs = queryVecs[:nq*dim]

	gt, err := loadIvecs(dataDir + "/sift_groundtruth.ivecs")
	if err != nil {
		t.Skipf("skip: cannot load groundtruth: %v", err)
	}
	gt = gt[:nq]

	// 构建 HNSW 索引
	t.Log("Building HNSW index (M=16, efSearch=128)...")
	buildStart := time.Now()

	hnsw := NewFlatHNSW(dim, 16, 128, 1.0/math.Log(16.0), MetricEuclidean)
	ids := make([]uint64, baseNv)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	if err := hnsw.Build(ids, baseVecs); err != nil {
		t.Fatalf("HNSW build: %v", err)
	}
	buildTime := time.Since(buildStart)
	t.Logf("HNSW build done in %v (%d nodes)", buildTime, hnsw.count)

	// 搜索 + 召回率 (efSearch=128)
	recalled10 := 0
	total10 := 0
	var latencies []float64

	for i := 0; i < nq; i++ {
		query := queryVecs[i*dim : (i+1)*dim]
		start := time.Now()

		results, err := hnsw.Search(query, 10)
		if err != nil {
			t.Fatalf("search query %d: %v", i, err)
		}

		lat := time.Since(start)
		latencies = append(latencies, float64(lat.Nanoseconds())/1000.0)

		resultIDs := make([]uint64, len(results))
		for j, r := range results {
			resultIDs[j] = r.ID
		}

		hit, total := computeRecall(resultIDs, gt[i], 10)
		recalled10 += hit
		total10 += total
	}

	recall := float64(recalled10) / float64(total10)
	p50, p99 := percentileFloat(latencies, 50), percentileFloat(latencies, 99)
	t.Logf("=== HNSW Search (M=16, efSearch=128) ===")
	t.Logf("Recall@10: %.2f%% (%d/%d)", recall*100, recalled10, total10)
	t.Logf("Latency  P50: %.1fµs  P99: %.1fµs", p50, p99)
	t.Logf("Avg: %.1fµs", avg(latencies))
	t.Logf("Build time: %v", buildTime)

	// 搜索 + 召回率 (efSearch=256, 无需重建)
	hnsw.EfSearch = 256
	recalled10b := 0
	total10b := 0
	var latenciesB []float64

	for i := 0; i < nq; i++ {
		query := queryVecs[i*dim : (i+1)*dim]
		start := time.Now()

		results, err := hnsw.Search(query, 10)
		if err != nil {
			t.Fatalf("search query %d: %v", i, err)
		}

		lat := time.Since(start)
		latenciesB = append(latenciesB, float64(lat.Nanoseconds())/1000.0)

		resultIDs := make([]uint64, len(results))
		for j, r := range results {
			resultIDs[j] = r.ID
		}

		hit, total := computeRecall(resultIDs, gt[i], 10)
		recalled10b += hit
		total10b += total
	}

	recallB := float64(recalled10b) / float64(total10b)
	p50b, p99b := percentileFloat(latenciesB, 50), percentileFloat(latenciesB, 99)
	t.Logf("=== HNSW Search (M=16, efSearch=256) ===")
	t.Logf("Recall@10: %.2f%% (%d/%d)", recallB*100, recalled10b, total10b)
	t.Logf("Latency  P50: %.1fµs  P99: %.1fµs", p50b, p99b)
	t.Logf("Avg: %.1fµs", avg(latenciesB))
}

// ---------------------------------------------------------------------------
// TestSIFT1M_IVFPQ: IVF-PQ 索引训练 + 搜索 + 召回率
// ---------------------------------------------------------------------------

func TestSIFT1M_IVFPQ(t *testing.T) {
	if testing.Short() {
		t.Skip("SIFT1M benchmark-style test skipped in short mode")
	}
	simd.Init()

	// 加载训练集
	learnVecs, dim, err := loadFvecs(dataDir + "/sift_learn.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_learn.fvecs: %v", err)
	}
	learnNv := len(learnVecs) / dim
	t.Logf("learn: %d vectors, dim=%d", learnNv, dim)

	// 训练 IVF-PQ
	t.Log("Training IVF-PQ (nc=256, nprobe=16, M=32, 8-bit)...")
	trainStart := time.Now()

	learnSlice := make([][]float32, learnNv)
	for i := 0; i < learnNv; i++ {
		learnSlice[i] = learnVecs[i*dim : (i+1)*dim]
	}

	ivfpq := NewIVFPQIndex(dim, 256, 16, 32, 8, 200)
	if err := ivfpq.Train(learnSlice); err != nil {
		t.Fatalf("IVF-PQ train: %v", err)
	}

	// 将 base 集分配到 IVF lists
	baseVecs, _, err := loadFvecs(dataDir + "/sift_base.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_base.fvecs: %v", err)
	}
	baseNv := len(baseVecs) / dim
	t.Logf("base: %d vectors", baseNv)

	// 构建 IVF lists
	for i := 0; i < baseNv; i++ {
		vec := baseVecs[i*dim : (i+1)*dim]
		bestD := float32(math.MaxFloat32)
		bestK := 0
		for j, c := range ivfpq.ivf.Centroids {
			d := simd.L2Sq(vec, c)
			if d < bestD {
				bestD = d
				bestK = j
			}
		}
		ivfpq.ivf.Lists[bestK] = append(ivfpq.ivf.Lists[bestK], i)
	}
	trainTime := time.Since(trainStart)
	t.Logf("IVF-PQ train+assign done in %v", trainTime)

	// 加载查询和 ground truth
	queryVecs, _, err := loadFvecs(dataDir + "/sift_query.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_query.fvecs: %v", err)
	}
	nq := 100
	queryVecs = queryVecs[:nq*dim]

	gt, err := loadIvecs(dataDir + "/sift_groundtruth.ivecs")
	if err != nil {
		t.Skipf("skip: cannot load groundtruth: %v", err)
	}
	gt = gt[:nq]

	// 构建 globalIDs（1-based）
	globalIDs := make([]uint64, baseNv)
	for i := range globalIDs {
		globalIDs[i] = uint64(i + 1)
	}

	// getVec 回调：从 baseVecs 获取向量
	getVec := func(localIdx int) []float32 {
		if localIdx < 0 || localIdx >= baseNv {
			return nil
		}
		return baseVecs[localIdx*dim : (localIdx+1)*dim]
	}

	// 搜索 + 召回率
	recalled10 := 0
	recalled100 := 0
	total10 := 0
	total100 := 0
	var latencies []float64

	for i := 0; i < nq; i++ {
		query := queryVecs[i*dim : (i+1)*dim]
		start := time.Now()

		results, err := ivfpq.Search(query, globalIDs, getVec, 100)
		if err != nil {
			t.Fatalf("search query %d: %v", i, err)
		}

		lat := time.Since(start)
		latencies = append(latencies, float64(lat.Microseconds()))

		resultIDs := make([]uint64, len(results))
		for j, r := range results {
			resultIDs[j] = r.GlobalID
		}

		hit10, total10v := computeRecall(resultIDs, gt[i], 10)
		hit100, total100v := computeRecall(resultIDs, gt[i], 100)
		recalled10 += hit10
		total10 += total10v
		recalled100 += hit100
		total100 += total100v
	}

	recall10 := float64(recalled10) / float64(total10)
	recall100 := float64(recalled100) / float64(total100)
	p50, p99 := percentileFloat(latencies, 50), percentileFloat(latencies, 99)
	t.Logf("=== IVF-PQ Search (nc=256, nprobe=16, M=32, 8-bit) ===")
	t.Logf("Recall@10:  %.2f%% (%d/%d)", recall10*100, recalled10, total10)
	t.Logf("Recall@100: %.2f%% (%d/%d)", recall100*100, recalled100, total100)
	t.Logf("Latency  P50: %.1fµs  P99: %.1fµs", p50, p99)
	t.Logf("Avg: %.1fµs", avg(latencies))
	t.Logf("Train+assign time: %v", trainTime)
}

// ---------------------------------------------------------------------------
// TestSIFT1M_HNSW_Mmap: HNSW 构建 + 持久化 + mmap 加载 + 搜索（验证零分配路径）
// ---------------------------------------------------------------------------

func TestSIFT1M_HNSW_Mmap(t *testing.T) {
	if testing.Short() {
		t.Skip("SIFT1M benchmark-style test skipped in short mode")
	}
	simd.Init()

	baseVecs, dim, err := loadFvecs(dataDir + "/sift_base.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_base.fvecs: %v", err)
	}
	baseNv := len(baseVecs) / dim

	queryVecs, _, err := loadFvecs(dataDir + "/sift_query.fvecs")
	if err != nil {
		t.Skipf("skip: cannot load sift_query.fvecs: %v", err)
	}
	nq := 50 // mmap 测试用 50 个查询
	queryVecs = queryVecs[:nq*dim]

	gt, err := loadIvecs(dataDir + "/sift_groundtruth.ivecs")
	if err != nil {
		t.Skipf("skip: cannot load groundtruth: %v", err)
	}
	gt = gt[:nq]

	// 构建
	t.Log("Building HNSW index...")
	hnsw := NewFlatHNSW(dim, 16, 64, 1.0/math.Log(16.0), MetricEuclidean)
	ids := make([]uint64, baseNv)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	if err := hnsw.Build(ids, baseVecs); err != nil {
		t.Fatalf("HNSW build: %v", err)
	}

	// 持久化到临时文件
	tmpFile, err := os.CreateTemp("", "hnsw_sift1m_*.bin")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	t.Log("Writing HNSW to disk...")
	if err := hnsw.WriteFile(tmpPath); err != nil {
		t.Fatalf("write: %v", err)
	}

	// mmap 加载
	t.Log("Loading HNSW via mmap...")
	mmapHnsw, err := MmapFlatHNSW(tmpPath, MetricEuclidean)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer mmapHnsw.Close()

	// 搜索 + 召回率
	recalled10 := 0
	total10 := 0
	var latencies []float64

	for i := 0; i < nq; i++ {
		query := queryVecs[i*dim : (i+1)*dim]
		start := time.Now()

		results, err := mmapHnsw.Search(query, 10)
		if err != nil {
			t.Fatalf("search query %d: %v", i, err)
		}

		lat := time.Since(start)
		latencies = append(latencies, float64(lat.Microseconds()))

		resultIDs := make([]uint64, len(results))
		for j, r := range results {
			resultIDs[j] = r.ID
		}

		hit, total := computeRecall(resultIDs, gt[i], 10)
		recalled10 += hit
		total10 += total
	}

	recall := float64(recalled10) / float64(total10)
	p50, p99 := percentileFloat(latencies, 50), percentileFloat(latencies, 99)
	t.Logf("=== HNSW Mmap Search (M=16, efSearch=64) ===")
	t.Logf("Recall@10: %.2f%% (%d/%d)", recall*100, recalled10, total10)
	t.Logf("Latency  P50: %.1fµs  P99: %.1fµs", p50, p99)
	t.Logf("Avg: %.1fµs", avg(latencies))
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

func percentileFloat(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	idx := int(float64(len(sorted)-1) * p / 100.0)
	return sorted[idx]
}

func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// 确保 vse 包被引用（用于 metric 类型）
var _ = vse.MetricEuclidean

// 确保 quantizer 包被引用（用于 IVF-PQ 训练）
var _ = quantizer.NewIVF
