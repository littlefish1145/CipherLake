package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"time"

	"nexus/internal/vector/vse/engine/index"
	"nexus/internal/vector/vse/simd"
)

func main() {
	dataDir := "data"
	if len(os.Args) > 1 {
		dataDir = os.Args[1]
	}
	outPath := dataDir + "/hnsw_index.bin"
	if len(os.Args) > 2 {
		outPath = os.Args[2]
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
	simd.Init()

	fmt.Println("Loading SIFT1M base vectors...")
	baseVecs, dim, err := loadFvecs(dataDir + "/sift_base.fvecs")
	if err != nil {
		panic(err)
	}
	baseNv := len(baseVecs) / dim
	fmt.Printf("Base: %d vectors, dim=%d\n", baseNv, dim)

	ids := make([]uint64, baseNv)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}

	fmt.Println("Building HNSW index (M=16, efConstruction=200)...")
	h := index.NewFlatHNSW(dim, 16, 200, 1.0/math.Log(float64(16)), index.MetricEuclidean)
	buildStart := time.Now()
	if err := h.Build(ids, baseVecs); err != nil {
		panic(err)
	}
	buildTime := time.Since(buildStart)
	fmt.Printf("Build done in %.1f seconds (%d nodes)\n", buildTime.Seconds(), h.Len())

	fmt.Printf("Writing to %s...\n", outPath)
	if err := h.WriteFile(outPath); err != nil {
		panic(err)
	}
	fmt.Println("Done!")
}

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
	vecs := make([]float32, n*dim)
	for i := 0; i < n; i++ {
		off := i*vecSize + 4
		for j := 0; j < dim; j++ {
			vecs[i*dim+j] = math.Float32frombits(binary.LittleEndian.Uint32(data[off+j*4:]))
		}
	}
	return vecs, dim, nil
}
