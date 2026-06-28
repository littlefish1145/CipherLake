package simd

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"unsafe"
)

func TestRuntimeInfo(t *testing.T) {
	Init()
	t.Logf("Runtime ISA: %s", RuntimeInfo.String())
	t.Logf("AVX2: %v, SSE4.2: %v, AVX512: %v, NEON: %v",
		RuntimeInfo.HasAVX2, RuntimeInfo.HasSSE42, RuntimeInfo.HasAVX512, RuntimeInfo.HasNEON)
	t.Logf("Dot func: %p, L2Sq func: %p", Dot, L2Sq)
	_ = fmt.Sprintf
}

func TestDot(t *testing.T) {
	Init()
	t.Logf("Dot func: %p", Dot)
	a := []float32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	b := []float32{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	got := Dot(a, b)
	var want float32
	for i := range a {
		want += a[i] * b[i]
	}
	t.Logf("Dot(%v, %v) = %f, want %f", a, b, got, want)
	if math.Abs(float64(got-want)) > 0.01 {
		t.Errorf("Dot = %f, want %f", got, want)
	}
}

func TestL2Sq(t *testing.T) {
	Init()
	a := []float32{1, 2, 3, 4}
	b := []float32{4, 3, 2, 1}
	got := L2Sq(a, b)
	var want float32
	for i := range a {
		d := a[i] - b[i]
		want += d * d
	}
	t.Logf("L2Sq = %f, want %f", got, want)
	if math.Abs(float64(got-want)) > 0.01 {
		t.Errorf("L2Sq = %f, want %f", got, want)
	}
}

func TestCosine(t *testing.T) {
	Init()
	a := []float32{1, 0, 0, 0}
	b := []float32{0, 1, 0, 0}
	got := Cosine(a, b)
	if math.Abs(float64(got-1.0)) > 0.01 {
		t.Errorf("Cosine(perp) = %f, want 1.0", got)
	}

	a = []float32{1, 0, 0, 0}
	b = []float32{1, 0, 0, 0}
	got = Cosine(a, b)
	if math.Abs(float64(got-0.0)) > 0.01 {
		t.Errorf("Cosine(ident) = %f, want 0.0", got)
	}
}

func TestBatchDot(t *testing.T) {
	Init()
	dim := 8
	count := 4
	query := []float32{1, 0, 0, 0, 0, 0, 0, 0}

	data := make([]float32, dim*count)
	for i := 0; i < count; i++ {
		data[i*dim] = float32(i + 1)
	}

	results := make([]float32, count)
	BatchDot(query, unsafe.Pointer(&data[0]), dim, count, results)

	for i := 0; i < count; i++ {
		want := float32(i + 1)
		if math.Abs(float64(results[i]-want)) > 0.01 {
			t.Errorf("BatchDot[%d] = %f, want %f", i, results[i], want)
		}
	}
}

func TestTopK(t *testing.T) {
	dists := []float32{5, 1, 4, 2, 3}
	scores, idx := TopK(dists, 3)
	expected := []float32{1, 2, 3}
	for i := range scores {
		if scores[i] != expected[i] {
			t.Errorf("TopK[%d] = %f, want %f", i, scores[i], expected[i])
		}
	}
	if idx[0] != 1 || idx[1] != 3 || idx[2] != 4 {
		t.Errorf("TopK indices = %v, want [1 3 4]", idx)
	}
}

func BenchmarkDot(b *testing.B) {
	Init()
	dim := 768
	avec := make([]float32, dim)
	bvec := make([]float32, dim)
	for i := range avec {
		avec[i] = rand.Float32()
		bvec[i] = rand.Float32()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Dot(avec, bvec)
	}
}

func BenchmarkL2Sq(b *testing.B) {
	Init()
	dim := 768
	avec := make([]float32, dim)
	bvec := make([]float32, dim)
	for i := range avec {
		avec[i] = rand.Float32()
		bvec[i] = rand.Float32()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		L2Sq(avec, bvec)
	}
}

func BenchmarkBatchDot(b *testing.B) {
	Init()
	dim := 768
	count := 1000
	query := make([]float32, dim)
	for i := range query {
		query[i] = rand.Float32()
	}

	data := make([]float32, dim*count)
	for i := range data {
		data[i] = rand.Float32()
	}

	results := make([]float32, count)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		BatchDot(query, unsafe.Pointer(&data[0]), dim, count, results)
	}
}

func TestDotGeneric(t *testing.T) {
	a := []float32{1, 2, 3, 4}
	b := []float32{5, 6, 7, 8}
	got := dotGeneric(a, b)
	var want float32
	for i := range a {
		want += a[i] * b[i]
	}
	if math.Abs(float64(got-want)) > 0.01 {
		t.Errorf("dotGeneric = %f, want %f", got, want)
	}
}

func TestDot4(t *testing.T) {
	Init()
	a := []float32{1, 2, 3, 4}
	b := []float32{5, 6, 7, 8}
	got := Dot(a, b)
	want := float32(70)
	t.Logf("Dot4 = %f, want %f", got, want)
	if math.Abs(float64(got-want)) > 0.01 {
		t.Errorf("Dot4 = %f, want %f", got, want)
	}
}

func TestDot16(t *testing.T) {
	Init()
	a := []float32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	b := []float32{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	got := Dot(a, b)
	var want float32
	for i := range a {
		want += a[i] * b[i]
	}
	t.Logf("Dot16 = %f, want %f", got, want)
	if math.Abs(float64(got-want)) > 0.01 {
		t.Errorf("Dot16 = %f, want %f", got, want)
	}
}
