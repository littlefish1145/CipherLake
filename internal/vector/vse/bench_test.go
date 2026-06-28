package vse

import (
	"fmt"
	"math/rand"
	"testing"
	"unsafe"

	"nexus/internal/vector/vse/simd"
)

func BenchmarkSimdDot(b *testing.B) {
	simd.Init()
	for _, dim := range []int{128, 768} {
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
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
		})
	}
}

func BenchmarkSimdL2Sq(b *testing.B) {
	simd.Init()
	for _, dim := range []int{128, 768} {
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
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
		})
	}
}

func BenchmarkSimdL2SqRaw(b *testing.B) {
	simd.Init()
	for _, dim := range []int{128, 768} {
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
			a := make([]float32, dim)
			bvec := make([]float32, dim)
			for i := range a {
				a[i] = rand.Float32()
				bvec[i] = rand.Float32()
			}
			aptr := unsafe.Pointer(&a[0])
			bptr := unsafe.Pointer(&bvec[0])
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simd.L2SqRaw(aptr, bptr, dim)
			}
		})
	}
}

func BenchmarkSimdDotRaw(b *testing.B) {
	simd.Init()
	for _, dim := range []int{128, 768} {
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
			a := make([]float32, dim)
			bvec := make([]float32, dim)
			for i := range a {
				a[i] = rand.Float32()
				bvec[i] = rand.Float32()
			}
			aptr := unsafe.Pointer(&a[0])
			bptr := unsafe.Pointer(&bvec[0])
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simd.DotRaw(aptr, bptr, dim)
			}
		})
	}
}

func BenchmarkSimdBatchL2(b *testing.B) {
	simd.Init()
	for _, dim := range []int{128, 768} {
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
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
		})
	}
}

func BenchmarkSimdBatchL2ByIndices(b *testing.B) {
	simd.Init()
	for _, dim := range []int{128, 768} {
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
			count := 1000
			query := make([]float32, dim)
			data := make([]float32, dim*count)
			indices := make([]int32, count)
			for i := range query {
				query[i] = rand.Float32()
			}
			for i := range data {
				data[i] = rand.Float32()
			}
			for i := range indices {
				indices[i] = int32(i)
			}
			results := make([]float32, count)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				simd.BatchL2SqByIndices(query, unsafe.Pointer(&data[0]), dim, indices, results)
			}
		})
	}
}

func BenchmarkSimdScanL2TopKByIndices(b *testing.B) {
	simd.Init()
	for _, dim := range []int{128, 768} {
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
			count := 1000
			query := make([]float32, dim)
			data := make([]float32, dim*count)
			indices := make([]int32, count)
			for i := range query {
				query[i] = rand.Float32()
			}
			for i := range data {
				data[i] = rand.Float32()
			}
			for i := range indices {
				indices[i] = int32(i)
			}
			var sel simd.TopKSelector
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sel.Init(10)
				simd.ScanL2TopKByIndices(query, unsafe.Pointer(&data[0]), dim, indices, &sel)
			}
		})
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
