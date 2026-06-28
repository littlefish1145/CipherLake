package simd

import "unsafe"

type BatchResult struct {
	Distances []float32
	Indices   []int32
}

func BatchDot(query []float32, vectors unsafe.Pointer, dim, count int, results []float32) {
	if len(results) < count {
		return
	}
	rowStride := dim * 4
	qptr := unsafe.Pointer(&query[0])
	for i := 0; i < count; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(rowStride))
		results[i] = DotRaw(qptr, row, dim)
	}
}

func BatchL2Sq(query []float32, vectors unsafe.Pointer, dim, count int, results []float32) {
	if len(results) < count {
		return
	}
	rowStride := dim * 4
	qptr := unsafe.Pointer(&query[0])
	for i := 0; i < count; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(rowStride))
		results[i] = L2SqRaw(qptr, row, dim)
	}
}

func BatchDotSoA(query unsafe.Pointer, vectors unsafe.Pointer, dim, count, stride int, results []float32) {
	if len(results) < count {
		return
	}
	qptr := query
	for i := 0; i < count; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(stride))
		results[i] = DotRaw(qptr, row, dim)
	}
}

func BatchL2SqSoA(query unsafe.Pointer, vectors unsafe.Pointer, dim, count, stride int, results []float32) {
	if len(results) < count {
		return
	}
	qptr := query
	for i := 0; i < count; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(stride))
		results[i] = L2SqRaw(qptr, row, dim)
	}
}

func Prefetch(addr unsafe.Pointer, level int) {
	switch level {
	case 0:
		prefetchL0(addr)
	case 1:
		prefetchL1(addr)
	case 2:
		prefetchL2(addr)
	default:
		prefetchNTA(addr)
	}
}

func prefetchL0(addr unsafe.Pointer)
func prefetchL1(addr unsafe.Pointer)
func prefetchL2(addr unsafe.Pointer)
func prefetchNTA(addr unsafe.Pointer)
