package main

import (
	"fmt"
	"math"
	"math/rand"

	"nexus/internal/vector/vse/engine/index"
	"nexus/internal/vector/vse/simd"
)

func main() {
	simd.Init()
	dim := 128
	n := 10000
	m := 16
	efConstruction := 200

	rng := rand.New(rand.NewSource(42))
	vectors := make([]float32, n*dim)
	for i := range vectors {
		vectors[i] = rng.Float32()
	}
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}

	fmt.Printf("Building HNSW (n=%d, M=%d, efConstruction=%d)...\n", n, m, efConstruction)
	h := index.NewFlatHNSW(dim, m, efConstruction, 1.0/math.Log(float64(m)), index.MetricEuclidean)
	if err := h.Build(ids, vectors); err != nil {
		panic(err)
	}
	fmt.Printf("Built %d nodes\n", h.Len())

	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32()
	}

	fmt.Println("\nBrute-force nearest neighbor search...")
	bfDists := make([]float64, n)
	for i := 0; i < n; i++ {
		off := i * dim
		d := float64(0)
		for j := 0; j < dim; j++ {
			diff := float64(query[j] - vectors[off+j])
			d += diff * diff
		}
		bfDists[i] = d
	}

	bfIdx := make([]int, n)
	for i := range bfIdx {
		bfIdx[i] = i
	}
	for i := 0; i < n-1; i++ {
		for j := i + 1; j < n; j++ {
			if bfDists[bfIdx[i]] > bfDists[bfIdx[j]] {
				bfIdx[i], bfIdx[j] = bfIdx[j], bfIdx[i]
			}
		}
	}

	for _, ef := range []int{64, 128, 256} {
		h.EfSearch = ef
		results, err := h.Search(query, 10)
		if err != nil {
			panic(err)
		}
		recalled := 0
		for k := 0; k < 10; k++ {
			for b := 0; b < 10; b++ {
				if results[k].ID == uint64(bfIdx[b]+1) {
					recalled++
					break
				}
			}
		}
		fmt.Printf("ef=%d: recall@10=%.3f (hits=%d/10)\n", ef, float64(recalled)/10, recalled)
	}

	fmt.Println("\nTop-10 brute-force IDs:", bfIdx[:10])
	for k := 0; k < 10; k++ {
		d := float64(0)
		off := (bfIdx[k]) * dim
		for j := 0; j < dim; j++ {
			diff := float64(query[j] - vectors[off+j])
			d += diff * diff
		}
		fmt.Printf("  BF[%d] = id=%d (dist=%.4f)\n", k, bfIdx[k]+1, d)
	}

	h.EfSearch = 64
	results, _ := h.Search(query, 10)
	for k := 0; k < 10; k++ {
		fmt.Printf("  HNSW[%d] = id=%d (dist=%.4f)\n", k, results[k].ID, results[k].Score)
	}
}
