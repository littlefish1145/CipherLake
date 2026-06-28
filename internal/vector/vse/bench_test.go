package vse

import (
	"math/rand"
	"testing"
	"unsafe"

	"nexus/internal/vector/vse/simd"
)

func BenchmarkSimdDot(b *testing.B) {
	simd.Init()
	dim := 768
	a := make([]float32, dim)
	bvec := make([]float32, dim)
	for i := range a {
		a[i] = rand.Float32()
		bvec[i] = rand.Float32()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		simd.Dot(a, bvec)
	}
}

func BenchmarkSimdL2(b *testing.B) {
	simd.Init()
	dim := 768
	a := make([]float32, dim)
	bvec := make([]float32, dim)
	for i := range a {
		a[i] = rand.Float32()
		bvec[i] = rand.Float32()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		simd.L2Sq(a, bvec)
	}
}

func BenchmarkSimdBatchDot(b *testing.B) {
	simd.Init()
	dim := 768
	count := 1000
	query := make([]float32, dim)
	data := make([]float32, dim*count)
	for i := range query {
		query[i] = rand.Float32()
	}
	for i := range data {
		data[i] = rand.Float32()
	}
	results := make([]float32, count)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		simd.BatchDot(query, unsafe.Pointer(&data[0]), dim, count, results)
	}
}

func BenchmarkSimdBatchL2(b *testing.B) {
	simd.Init()
	dim := 768
	count := 1000
	query := make([]float32, dim)
	data := make([]float32, dim*count)
	for i := range query {
		query[i] = rand.Float32()
	}
	for i := range data {
		data[i] = rand.Float32()
	}
	results := make([]float32, count)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		simd.BatchL2Sq(query, unsafe.Pointer(&data[0]), dim, count, results)
	}
}

func BenchmarkTopK(b *testing.B) {
	n := 10000
	dists := make([]float32, n)
	for i := range dists {
		dists[i] = rand.Float32()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		simd.TopK(dists, 10)
	}
}

func BenchmarkTopKSelector(b *testing.B) {
	n := 10000
	dists := make([]float32, n)
	for i := range dists {
		dists[i] = rand.Float32()
	}
	results := make([]float32, 10)
	resultIdxs := make([]int32, 10)
	var sel simd.TopKSelector

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sel.Init(10)
		for j, d := range dists {
			sel.Push(d, int32(j))
		}
		sel.WriteResults(results, resultIdxs)
	}
}

func BenchmarkTopKNaive(b *testing.B) {
	n := 10000
	dists := make([]float32, n)
	for i := range dists {
		dists[i] = rand.Float32()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		simd.TopK(dists, 10)
	}
}


