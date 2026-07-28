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
	t.Logf("DotRaw func: %p, L2SqRaw func: %p", DotRaw, L2SqRaw)
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

func TestBatchL2SqStride(t *testing.T) {
	Init()
	dim := 2
	stride := 16
	query := []float32{1, 2}
	buf := make([]byte, stride*3)

	setVec := func(i int, a, b float32) {
		base := i * stride
		*(*float32)(unsafe.Pointer(&buf[base])) = a
		*(*float32)(unsafe.Pointer(&buf[base+4])) = b
	}
	setVec(0, 1, 2)
	setVec(1, 2, 2)
	setVec(2, 1, 4)

	results := make([]float32, 3)
	BatchL2SqStride(query, unsafe.Pointer(&buf[0]), dim, 3, stride, results)

	want := []float32{0, 1, 4}
	for i := range want {
		if math.Abs(float64(results[i]-want[i])) > 0.01 {
			t.Errorf("BatchL2SqStride[%d] = %f, want %f", i, results[i], want[i])
		}
	}
}

func TestScanL2TopKStride(t *testing.T) {
	Init()
	dim := 2
	stride := 16
	query := []float32{1, 2}
	buf := make([]byte, stride*4)

	setVec := func(i int, a, b float32) {
		base := i * stride
		*(*float32)(unsafe.Pointer(&buf[base])) = a
		*(*float32)(unsafe.Pointer(&buf[base+4])) = b
	}
	setVec(0, 1, 2)
	setVec(1, 4, 2)
	setVec(2, 2, 2)
	setVec(3, 1, 5)

	var sel TopKSelector
	sel.Init(2)
	ScanL2TopKStride(query, unsafe.Pointer(&buf[0]), dim, 4, stride, &sel)

	scores, idxs := sel.Result()
	wantScores := []float32{0, 1}
	wantIdxs := []int32{0, 2}
	for i := range wantScores {
		if math.Abs(float64(scores[i]-wantScores[i])) > 0.01 {
			t.Errorf("ScanL2TopKStride score[%d] = %f, want %f", i, scores[i], wantScores[i])
		}
		if idxs[i] != wantIdxs[i] {
			t.Errorf("ScanL2TopKStride idx[%d] = %d, want %d", i, idxs[i], wantIdxs[i])
		}
	}
}

func TestScanL2TopKByIndices(t *testing.T) {
	Init()
	dim := 2
	data := []float32{
		1, 2,
		4, 2,
		2, 2,
		1, 5,
	}
	query := []float32{1, 2}
	indices := []int32{3, 1, -1, 2, 0}

	var sel TopKSelector
	sel.Init(3)
	ScanL2TopKByIndices(query, unsafe.Pointer(&data[0]), dim, indices, &sel)

	scores, idxs := sel.Result()
	wantScores := []float32{0, 1, 9}
	for i := range wantScores {
		if math.Abs(float64(scores[i]-wantScores[i])) > 0.01 {
			t.Errorf("ScanL2TopKByIndices score[%d] = %f, want %f", i, scores[i], wantScores[i])
		}
	}
	if idxs[0] != 0 || idxs[1] != 2 {
		t.Errorf("ScanL2TopKByIndices leading idxs = %v, want [0 2 ...]", idxs)
	}
	if idxs[2] != 1 && idxs[2] != 3 {
		t.Errorf("ScanL2TopKByIndices tail idx = %d, want 1 or 3", idxs[2])
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

func BenchmarkL2SqRaw(b *testing.B) {
	Init()
	dim := 768
	avec := make([]float32, dim)
	bvec := make([]float32, dim)
	for i := range avec {
		avec[i] = rand.Float32()
		bvec[i] = rand.Float32()
	}
	aptr := unsafe.Pointer(&avec[0])
	bptr := unsafe.Pointer(&bvec[0])

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		L2SqRaw(aptr, bptr, dim)
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
