package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"time"

	"nexus/internal/vector/vse/engine/index"
)

func main() {
	dataFile := flag.String("data", "", "path to binary vector file (float32, dim*N bytes)")
	dim := flag.Int("dim", 1024, "vector dimension")
	n := flag.Int("n", 5000, "number of vectors (generated if no data file)")
	m := flag.Int("m", 16, "HNSW M parameter")
	efSearch := flag.Int("ef", 64, "HNSW efSearch parameter")
	ml := flag.Float64("ml", 0.5, "HNSW ml parameter")
	topK := flag.Int("topk", 10, "search top-K")
	queries := flag.Int("queries", 100, "number of query vectors")
	graphFile := flag.String("graph", "vectest_graph.bin", "path for graph persistence")
	flag.Parse()

	fmt.Printf("dim=%d  n=%d  M=%d  efSearch=%d  ml=%.2f  topK=%d  queries=%d\n",
		*dim, *n, *m, *efSearch, *ml, *topK, *queries)
	fmt.Println()

	var vectors [][]float32
	var ids []uint64

	if *dataFile != "" {
		log.Printf("reading vectors from %s ...", *dataFile)
		v, err := readVectors(*dataFile, *dim)
		if err != nil {
			log.Fatalf("read vectors: %v", err)
		}
		vectors = v
		*n = len(v)
		log.Printf("loaded %d vectors", *n)
	} else {
		log.Printf("generating %d random %d-d vectors ...", *n, *dim)
		rng := rand.New(rand.NewSource(42))
		vectors = make([][]float32, *n)
		for i := range vectors {
			v := make([]float32, *dim)
			for j := range v {
				v[j] = rng.Float32()
			}
			vectors[i] = v
		}
	}

	ids = make([]uint64, *n)
	for i := range ids {
		ids[i] = uint64(i)
	}

	idx := index.NewFlatHNSW(*dim, *m, *efSearch, *ml, index.MetricCosine)

	buildStart := time.Now()
	for i, v := range vectors {
		if err := idx.Insert(ids[i], v); err != nil {
			log.Fatalf("insert %d: %v", i, err)
		}
	}
	buildTime := time.Since(buildStart)
	fmt.Printf("build: %d vectors in %v  (%.0f vec/s)\n", *n, buildTime, float64(*n)/buildTime.Seconds())

	persistStart := time.Now()
	if err := idx.WriteFile(*graphFile); err != nil {
		log.Fatalf("write graph: %v", err)
	}
	persistTime := time.Since(persistStart)
	fmt.Printf("persist: %v  (%d bytes)\n", persistTime, idx.FileSize())
	fmt.Println()

	loadStart := time.Now()
	idx2, err := index.MmapFlatHNSW(*graphFile, index.MetricCosine)
	if err != nil {
		log.Fatalf("mmap graph: %v", err)
	}
	loadTime := time.Since(loadStart)
	fmt.Printf("mmap load: %v\n", loadTime)
	defer idx2.Close()

	var queryVecs [][]float32
	if *dataFile != "" {
		rng := rand.New(rand.NewSource(99))
		queryVecs = make([][]float32, *queries)
		for i := range queryVecs {
			v := make([]float32, *dim)
			for j := range v {
				v[j] = rng.Float32()
			}
			queryVecs[i] = v
		}
	} else {
		rng := rand.New(rand.NewSource(99))
		queryVecs = make([][]float32, *queries)
		for i := range queryVecs {
			v := make([]float32, *dim)
			for j := range v {
				v[j] = rng.Float32()
			}
			queryVecs[i] = v
		}
	}

	searchTimes := make([]time.Duration, *queries)
	totalHnsw := 0
	for i, q := range queryVecs {
		start := time.Now()
		results, err := idx2.Search(q, *topK)
		elapsed := time.Since(start)
		searchTimes[i] = elapsed
		if err != nil {
			log.Fatalf("search %d: %v", i, err)
		}
		totalHnsw += len(results)
	}

	var sum time.Duration
	for _, d := range searchTimes {
		sum += d
	}
	avg := sum / time.Duration(*queries)

	fmt.Printf("search %d queries, top-%d:\n", *queries, *topK)
	fmt.Printf("  avg latency: %v\n", avg)
	fmt.Printf("  min latency: %v\n", minDuration(searchTimes))
	fmt.Printf("  max latency: %v\n", maxDuration(searchTimes))
	fmt.Println()

	log.Print("validating recall against brute-force ...")
	recallHits := 0
	totalExpected := 0
	for _, q := range queryVecs {
		bfResults := bruteForce(vectors, q, *topK)
		bfSet := make(map[uint64]struct{})
		for _, r := range bfResults {
			bfSet[r.ID] = struct{}{}
		}
		hnswResults, _ := idx2.Search(q, *topK)
		for _, r := range hnswResults {
			if _, ok := bfSet[r.ID]; ok {
				recallHits++
			}
		}
		totalExpected += *topK
	}
	recall := float64(recallHits) / float64(totalExpected) * 100
	fmt.Printf("recall@%d: %.1f%%\n", *topK, recall)

	if err := os.Remove(*graphFile); err != nil {
		log.Printf("cleanup: %v", err)
	}
}

func readVectors(path string, dim int) ([][]float32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vecBytes := dim * 4
	n := len(data) / vecBytes
	if len(data)%vecBytes != 0 {
		return nil, fmt.Errorf("file size %d not a multiple of vector size %d", len(data), vecBytes)
	}
	vectors := make([][]float32, n)
	for k := range vectors {
		v := make([]float32, dim)
		for j := range v {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(data[k*vecBytes+j*4:]))
		}
		vectors[k] = v
	}
	return vectors, nil
}

func bruteForce(vectors [][]float32, query []float32, topK int) []index.HNSWSearchResult {
	type neighbor struct {
		id  uint64
		dist float32
	}
	nbrs := make([]neighbor, len(vectors))
	for i, v := range vectors {
		nbrs[i] = neighbor{id: uint64(i), dist: cosineDist(v, query)}
	}

	for i := 1; i < len(nbrs); i++ {
		v := nbrs[i]
		j := i - 1
		for j >= 0 && nbrs[j].dist > v.dist {
			nbrs[j+1] = nbrs[j]
			j--
		}
		nbrs[j+1] = v
	}

	k := topK
	if k > len(nbrs) {
		k = len(nbrs)
	}
	results := make([]index.HNSWSearchResult, k)
	for i := 0; i < k; i++ {
		results[i] = index.HNSWSearchResult{ID: nbrs[i].id, Score: nbrs[i].dist}
	}
	return results
}

func cosineDist(a, b []float32) float32 {
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
}

func minDuration(d []time.Duration) time.Duration {
	m := d[0]
	for _, v := range d[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxDuration(d []time.Duration) time.Duration {
	m := d[0]
	for _, v := range d[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
