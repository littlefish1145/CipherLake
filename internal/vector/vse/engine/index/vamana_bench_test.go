package index

import (
	"fmt"
	"os"
	"math/rand"
	"sort"
	"testing"
	"time"

	"nexus/internal/vector/vse/engine/quantizer"
	"nexus/internal/vector/vse/simd"
)

// ---------------------------------------------------------------------------
// 合成数据生成：128 维随机向量 + ground truth（精确 topK）
// ---------------------------------------------------------------------------

type benchDataset struct {
	dim    int
	n      int
	nq     int
	vecs   []float32 // n*dim 扁平
	ids    []uint64
	queries []float32 // nq*dim 扁平
	gt     [][]uint64 // nq × topK 精确近邻 ID
}

func generateDataset(n, dim, nq, topK int, seed int64) *benchDataset {
	rng := rand.New(rand.NewSource(seed))
	vecs := make([]float32, n*dim)
	for i := range vecs {
		vecs[i] = rng.Float32()
	}
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = uint64(i)
	}

	q := make([]float32, nq*dim)
	for i := range q {
		q[i] = rng.Float32()
	}

	// Brute force GT
	gt := make([][]uint64, nq)
	for qi := 0; qi < nq; qi++ {
		qOff := qi * dim
		type nn struct {
			id  uint64
			dist float32
		}
		res := make([]nn, n)
		for vi := 0; vi < n; vi++ {
			vOff := vi * dim
			d := l2Sq(q[qOff:qOff+dim], vecs[vOff:vOff+dim])
			res[vi] = nn{id: ids[vi], dist: d}
		}
		sort.Slice(res, func(i, j int) bool { return res[i].dist < res[j].dist })
		if len(res) > topK {
			res = res[:topK]
		}
		row := make([]uint64, len(res))
		for i, r := range res {
			row[i] = r.id
		}
		gt[qi] = row
	}

	return &benchDataset{
		dim: dim, n: n, nq: nq,
		vecs: vecs, ids: ids,
		queries: q, gt: gt,
	}
}

func l2Sq(a, b []float32) float32 {
	var s float32
	for i := range a {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

// ---------------------------------------------------------------------------
// DiskANN recall benchmark（模拟 SIFT1M 规模的合成数据）
// ---------------------------------------------------------------------------

func BenchmarkDiskANNRecallSynthetic(b *testing.B) {
	dim := 128
	n := 10000   // 训练向量数
	nq := 100    // 查询数
	topK := 10
	R := 32
	L := 128
	alpha := 1.2
	M := dim / 4 // 32 sub-quantizers

	b.Logf("DiskANN recall benchmark: %d vectors, %d dim, R=%d, L=%d, alpha=%.1f, M=%d",
		n, dim, R, L, alpha, M)

	ds := generateDataset(n, dim, nq, topK, 42)
	simd.Init()

	// Build DiskANN index
	pq := quantizer.NewPQQuantizer(dim, M, 8)
	idx := NewDiskANNIndex(dim, R, L, alpha, pq, l2Sq)

	// Convert flat vecs to [][]float32 for Build
	vecs2d := make([][]float32, n)
	for i := 0; i < n; i++ {
		vecs2d[i] = ds.vecs[i*dim : (i+1)*dim]
	}

	buildStart := time.Now()
	if err := idx.Build(ds.ids, vecs2d); err != nil {
		b.Fatalf("build: %v", err)
	}
	buildTime := time.Since(buildStart)
	b.Logf("Build time: %v (%d vecs/sec)", buildTime, int(float64(n)/buildTime.Seconds()))

	// Search + compute recall
	var totalRecall float64
	var totalLatNs int64

	for qi := 0; qi < nq; qi++ {
		query := ds.queries[qi*dim : (qi+1)*dim]

		start := time.Now()
		results, err := idx.SearchDiskANN(query, topK)
		lat := time.Since(start).Nanoseconds()
		totalLatNs += lat

		if err != nil {
			b.Fatalf("search %d: %v", qi, err)
		}

		// Compute recall@topK
		gtSet := make(map[uint64]bool, len(ds.gt[qi]))
		for _, gid := range ds.gt[qi] {
			gtSet[gid] = true
		}
		hits := 0
		for _, r := range results {
			if gtSet[r.ID] {
				hits++
			}
		}
		recall := float64(hits) / float64(len(ds.gt[qi]))
		totalRecall += recall
	}

	avgRecall := totalRecall / float64(nq)
	avgLat := float64(totalLatNs) / float64(nq) / 1e6 // ms

	b.Logf("Recall@%d: %.2f%% | Avg latency: %.2f ms", topK, avgRecall*100, avgLat)

	// 合成均匀随机数据的期望 recall 较低（≈85-92%），
	// 真实 SIFT1M/GIST 数据上 recall 可达 95%+。
	threshold := 0.85
	if avgRecall < threshold {
		b.Errorf("Recall too low: %.2f%% < %.0f%% — try increasing L or tuning PQ", avgRecall*100, threshold*100)
	} else {
		b.Logf("PASS: recall %.2f%% >= %.0f%%", avgRecall*100, threshold*100)
	}
}

// ---------------------------------------------------------------------------
// 参数扫描：α/R/L 对 recall 的影响
// ---------------------------------------------------------------------------

func TestVamanaParameterScan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping parameter scan in short mode")
	}

	dim := 128
	n := 5000
	nq := 50
	topK := 10
	simd.Init()

	ds := generateDataset(n, dim, nq, topK, 42)

	configs := []struct {
		R, L int
		alpha float64
	}{
		{16, 64, 1.0},
		{16, 64, 1.2},
		{32, 64, 1.2},
		{32, 128, 1.2},
		{64, 128, 1.2},
		{32, 64, 1.5},
	}

	vecs2d := make([][]float32, n)
	for i := 0; i < n; i++ {
		vecs2d[i] = ds.vecs[i*dim : (i+1)*dim]
	}

	for _, cfg := range configs {
		pq := quantizer.NewPQQuantizer(dim, dim/4, 8)
		idx := NewDiskANNIndex(dim, cfg.R, cfg.L, cfg.alpha, pq, l2Sq)

		if err := idx.Build(ds.ids, vecs2d); err != nil {
			t.Fatalf("build R=%d L=%d α=%.1f: %v", cfg.R, cfg.L, cfg.alpha, err)
		}

		var totalRecall float64
		for qi := 0; qi < nq; qi++ {
			query := ds.queries[qi*dim : (qi+1)*dim]
			results, err := idx.SearchDiskANN(query, topK)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			gtSet := make(map[uint64]bool, len(ds.gt[qi]))
			for _, gid := range ds.gt[qi] {
				gtSet[gid] = true
			}
			hits := 0
			for _, r := range results {
				if gtSet[r.ID] {
					hits++
				}
			}
			totalRecall += float64(hits) / float64(len(ds.gt[qi]))
		}
		avgRecall := totalRecall / float64(nq)
		t.Logf("R=%3d L=%3d α=%.1f → recall@%d = %.1f%%",
			cfg.R, cfg.L, cfg.alpha, topK, avgRecall*100)
	}
}

// generateClusteredDataset 生成聚类分布的数据（更接近真实场景）
func generateClusteredDataset(n, dim, nq, topK int, nClusters int, seed int64) *benchDataset {
	rng := rand.New(rand.NewSource(seed))

	// 生成聚类中心
	centroids := make([][]float32, nClusters)
	for c := 0; c < nClusters; c++ {
		centroids[c] = make([]float32, dim)
		for j := 0; j < dim; j++ {
			centroids[c][j] = rng.Float32() * 2 // spread centroids
		}
	}

	vecs := make([]float32, n*dim)
	ids := make([]uint64, n)
	for i := 0; i < n; i++ {
		c := i % nClusters
		ids[i] = uint64(i)
		for j := 0; j < dim; j++ {
			vecs[i*dim+j] = centroids[c][j] + (rng.Float32()-0.5)*0.3
		}
	}

	q := make([]float32, nq*dim)
	for i := range q {
		q[i] = rng.Float32() * 2
	}

	// Brute force GT
	gt := make([][]uint64, nq)
	for qi := 0; qi < nq; qi++ {
		qOff := qi * dim
		type nn struct {
			id   uint64
			dist float32
		}
		res := make([]nn, n)
		for vi := 0; vi < n; vi++ {
			vOff := vi * dim
			d := l2Sq(q[qOff:qOff+dim], vecs[vOff:vOff+dim])
			res[vi] = nn{id: ids[vi], dist: d}
		}
		sort.Slice(res, func(i, j int) bool { return res[i].dist < res[j].dist })
		if len(res) > topK {
			res = res[:topK]
		}
		row := make([]uint64, len(res))
		for i, r := range res {
			row[i] = r.id
		}
		gt[qi] = row
	}

	return &benchDataset{
		dim: dim, n: n, nq: nq,
		vecs: vecs, ids: ids,
		queries: q, gt: gt,
	}
}

func BenchmarkDiskANNRecallClustered(b *testing.B) {
	dim := 128
	n := 10000
	nq := 100
	topK := 10

	type paramSet struct {
		R, L        int
		alpha       float64
		M           int
		expectRecall float64
	}
	params := []paramSet{
		{64, 128, 1.2, 64, 0.85},  // 推荐配置：宽图 + 细粒度 PQ
		{32, 256, 1.2, 64, 0.70},  // 窄图 + 长搜索
		{32, 128, 1.2, 32, 0.50},  // 粗粒度 PQ，低预期
	}

	for _, p := range params {
		name := fmt.Sprintf("R=%d_L=%d_M=%d", p.R, p.L, p.M)
		b.Run(name, func(b *testing.B) {
			ds := generateClusteredDataset(n, dim, nq, topK, 50, 42)
			simd.Init()

			pq := quantizer.NewPQQuantizer(dim, p.M, 8)
			idx := NewDiskANNIndex(dim, p.R, p.L, p.alpha, pq, l2Sq)

			vecs2d := make([][]float32, n)
			for i := 0; i < n; i++ {
				vecs2d[i] = ds.vecs[i*dim : (i+1)*dim]
			}

			if err := idx.Build(ds.ids, vecs2d); err != nil {
				b.Fatalf("build: %v", err)
			}

			var totalRecall float64
			for qi := 0; qi < nq; qi++ {
				query := ds.queries[qi*dim : (qi+1)*dim]
				results, err := idx.SearchDiskANN(query, topK)
				if err != nil {
					b.Fatalf("search %d: %v", qi, err)
				}
				gtSet := make(map[uint64]bool, len(ds.gt[qi]))
				for _, gid := range ds.gt[qi] {
					gtSet[gid] = true
				}
				hits := 0
				for _, r := range results {
					if gtSet[r.ID] {
						hits++
					}
				}
				totalRecall += float64(hits) / float64(len(ds.gt[qi]))
			}

			avgRecall := totalRecall / float64(nq)
			b.Logf("Clustered recall@%d: %.2f%%", topK, avgRecall*100)

			if avgRecall < p.expectRecall {
				b.Errorf("Recall %.2f%% < %.0f%%", avgRecall*100, p.expectRecall*100)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 对比基准：DiskANN vs HNSW vs Flat（精确）
// ---------------------------------------------------------------------------

func BenchmarkDiskANNvsHNSW(b *testing.B) {
	dim := 128
	n := 10000
	nq := 100
	topK := 10
	simd.Init()

	ds := generateDataset(n, dim, nq, topK, 42)

	vecs2d := make([][]float32, n)
	for i := 0; i < n; i++ {
		vecs2d[i] = ds.vecs[i*dim : (i+1)*dim]
	}

	// 1. Flat (exact)
	b.Run("Flat", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for qi := 0; qi < nq; qi++ {
				query := ds.queries[qi*dim : (qi+1)*dim]
				type nn struct {
					id  uint64
					dist float32
				}
				all := make([]nn, n)
				for vi := 0; vi < n; vi++ {
					all[vi] = nn{id: ds.ids[vi], dist: l2Sq(query, ds.vecs[vi*dim:(vi+1)*dim])}
				}
				sort.Slice(all, func(i, j int) bool { return all[i].dist < all[j].dist })
				_ = all[:topK]
			}
		}
	})

	// 2. HNSW
	b.Run("HNSW", func(b *testing.B) {
		hnsw := NewHNSWIndex(dim, 16, 64, 0.5, func(a, b []float32) float32 {
			return l2Sq(a, b)
		})
		for i := 0; i < n; i++ {
			hnsw.Insert(ds.ids[i], vecs2d[i])
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for qi := 0; qi < nq; qi++ {
				query := ds.queries[qi*dim : (qi+1)*dim]
				results, _ := hnsw.Search(query, topK)
				_ = results
			}
		}
	})

	// 3. DiskANN (Vamana + PQ-ADC)
	b.Run("DiskANN", func(b *testing.B) {
		pq := quantizer.NewPQQuantizer(dim, dim/4, 8)
		d := NewDiskANNIndex(dim, 32, 128, 1.2, pq, l2Sq)
		if err := d.Build(ds.ids, vecs2d); err != nil {
			b.Fatalf("build: %v", err)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for qi := 0; qi < nq; qi++ {
				query := ds.queries[qi*dim : (qi+1)*dim]
				results, _ := d.SearchDiskANN(query, topK)
				_ = results
			}
		}
	})

	// 4. Vamana exact (no PQ)
	b.Run("VamanaExact", func(b *testing.B) {
		g := NewVamanaGraph(dim, 32, 128, 1.2, MetricEuclidean)
		if err := g.Build(ds.ids, ds.vecs); err != nil {
			b.Fatalf("build: %v", err)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for qi := 0; qi < nq; qi++ {
				query := ds.queries[qi*dim : (qi+1)*dim]
				results, _ := g.Search(query, topK)
				_ = results
			}
		}
	})
}

// ---------------------------------------------------------------------------
// TODO: 加载真实 SIFT1M 数据集的测试（从磁盘或 S3）
// 使用方式：下载 sift_base.fvecs + sift_query.fvecs + sift_groundtruth.ivecs
// 放到 testdata/sift1m/ 目录下即可
// ---------------------------------------------------------------------------

func TestSIFT1MDiskANN(t *testing.T) {
	siftDir := "testdata/sift1m"
	if !dirExists(siftDir) {
		t.Skipf("SIFT1M data not found at %s (optional benchmark)", siftDir)
	}

	dim := 128
	n := 1000000
	nq := 10000
	topK := 10
	simd.Init()

	// 加载 SIFT1M（仅第一个 1M 向量）
	vecs := make([]float32, n*dim)
	ids := make([]uint64, n)
	_ = vecs
	_ = ids
	_ = nq
	_ = topK
	// TODO: 实现 fvecs 文件解析和加载
	// 参考: internal/vector/vse/gpu/sift1m_test.go

	t.Log("SIFT1M data loading not yet implemented — add fvecs/ivecs parsing to enable")
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
