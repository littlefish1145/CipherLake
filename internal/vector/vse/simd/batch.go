package simd

import "unsafe"

type BatchResult struct {
	Distances []float32
	Indices   []int32
}

const batchPrefetchDistance = 2

func batchL2Sq4Contig(qptr, vectors unsafe.Pointer, dim int, results []float32) {
	L2SqBatch4ContigRaw(qptr, vectors, dim, unsafe.Pointer(&results[0]))
}

func batchL2Sq4Stride(qptr, vectors unsafe.Pointer, dim, stride int, results []float32) {
	L2SqBatch4StrideRaw(qptr, vectors, dim, stride, unsafe.Pointer(&results[0]))
}

func l2SqBatch4ContigFallback(qptr, vectors unsafe.Pointer, dim int, out unsafe.Pointer) {
	results := unsafe.Slice((*float32)(out), 4)
	rowStride := dim * 4
	for i := 0; i < 4; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(rowStride))
		results[i] = L2SqRaw(qptr, row, dim)
	}
}

func l2SqBatch4StrideFallback(qptr, vectors unsafe.Pointer, dim, stride int, out unsafe.Pointer) {
	results := unsafe.Slice((*float32)(out), 4)
	for i := 0; i < 4; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(stride))
		results[i] = L2SqRaw(qptr, row, dim)
	}
}

func BatchDot(query []float32, vectors unsafe.Pointer, dim, count int, results []float32) {
	if len(query) == 0 || len(results) < count {
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
	if len(query) == 0 || len(results) < count {
		return
	}
	rowStride := dim * 4
	qptr := unsafe.Pointer(&query[0])
	i := 0
	for ; i+4 <= count; i += 4 {
		batchL2Sq4Contig(qptr, unsafe.Add(vectors, uintptr(i)*uintptr(rowStride)), dim, results[i:i+4])
	}
	for ; i < count; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(rowStride))
		results[i] = L2SqRaw(qptr, row, dim)
	}
}

func BatchDotStride(query []float32, vectors unsafe.Pointer, dim, count, stride int, results []float32) {
	if len(query) == 0 || len(results) < count {
		return
	}
	qptr := unsafe.Pointer(&query[0])
	for i := 0; i < count; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(stride))
		results[i] = DotRaw(qptr, row, dim)
	}
}

func BatchL2SqStride(query []float32, vectors unsafe.Pointer, dim, count, stride int, results []float32) {
	if len(query) == 0 || len(results) < count {
		return
	}
	qptr := unsafe.Pointer(&query[0])
	i := 0
	for ; i+4 <= count; i += 4 {
		batchL2Sq4Stride(qptr, unsafe.Add(vectors, uintptr(i)*uintptr(stride)), dim, stride, results[i:i+4])
	}
	for ; i < count; i++ {
		row := unsafe.Add(vectors, uintptr(i)*uintptr(stride))
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

func BatchDotByIndices(query []float32, vectors unsafe.Pointer, dim int, indices []int32, results []float32) {
	if len(query) == 0 || len(results) < len(indices) {
		return
	}
	rowStride := dim * 4
	qptr := unsafe.Pointer(&query[0])
	for i, idx := range indices {
		if idx < 0 {
			continue
		}
		if j := i + batchPrefetchDistance; j < len(indices) {
			nextIdx := indices[j]
			if nextIdx >= 0 {
				next := unsafe.Add(vectors, uintptr(nextIdx)*uintptr(rowStride))
				Prefetch(next, 0)
			}
		}
		row := unsafe.Add(vectors, uintptr(idx)*uintptr(rowStride))
		results[i] = DotRaw(qptr, row, dim)
	}
}

func BatchL2SqByIndices(query []float32, vectors unsafe.Pointer, dim int, indices []int32, results []float32) {
	if len(query) == 0 || len(results) < len(indices) {
		return
	}
	rowStride := dim * 4
	qptr := unsafe.Pointer(&query[0])
	for i := 0; i < len(indices); {
		if i+4 <= len(indices) &&
			indices[i] >= 0 &&
			indices[i+1] == indices[i]+1 &&
			indices[i+2] == indices[i]+2 &&
			indices[i+3] == indices[i]+3 {
			base := unsafe.Add(vectors, uintptr(indices[i])*uintptr(rowStride))
			L2SqBatch4ContigRaw(qptr, base, dim, unsafe.Pointer(&results[i]))
			i += 4
			continue
		}
		idx := indices[i]
		if idx < 0 {
			i++
			continue
		}
		if j := i + batchPrefetchDistance; j < len(indices) {
			nextIdx := indices[j]
			if nextIdx >= 0 {
				next := unsafe.Add(vectors, uintptr(nextIdx)*uintptr(rowStride))
				Prefetch(next, 0)
			}
		}
		row := unsafe.Add(vectors, uintptr(idx)*uintptr(rowStride))
		results[i] = L2SqRaw(qptr, row, dim)
		i++
	}
}

func ScanL2TopKStride(query []float32, vectors unsafe.Pointer, dim, count, stride int, sel *TopKSelector) {
	if len(query) == 0 || count <= 0 || sel == nil {
		return
	}
	qptr := unsafe.Pointer(&query[0])
	var block [256]float32
	for offset := 0; offset < count; offset += len(block) {
		n := count - offset
		if n > len(block) {
			n = len(block)
		}
		chunkBase := unsafe.Add(vectors, uintptr(offset)*uintptr(stride))
		i := 0
		for ; i+4 <= n; i += 4 {
			batchL2Sq4Stride(qptr, unsafe.Add(chunkBase, uintptr(i)*uintptr(stride)), dim, stride, block[i:i+4])
		}
		for ; i < n; i++ {
			row := unsafe.Add(chunkBase, uintptr(i)*uintptr(stride))
			block[i] = L2SqRaw(qptr, row, dim)
		}
		sel.PushBatch(block[:n], int32(offset))
	}
}

func ScanL2TopKByIndices(query []float32, vectors unsafe.Pointer, dim int, indices []int32, sel *TopKSelector) {
	if len(query) == 0 || len(indices) == 0 || sel == nil {
		return
	}
	rowStride := dim * 4
	qptr := unsafe.Pointer(&query[0])
	var tmp [4]float32
	for offset := 0; offset < len(indices); offset += 256 {
		n := len(indices) - offset
		if n > 256 {
			n = 256
		}
		for i := 0; i < n; {
			if i+4 <= n &&
				indices[offset+i] >= 0 &&
				indices[offset+i+1] == indices[offset+i]+1 &&
				indices[offset+i+2] == indices[offset+i]+2 &&
				indices[offset+i+3] == indices[offset+i]+3 {
				baseIdx := indices[offset+i]
				base := unsafe.Add(vectors, uintptr(baseIdx)*uintptr(rowStride))
				L2SqBatch4ContigRaw(qptr, base, dim, unsafe.Pointer(&tmp[0]))
				sel.Push(tmp[0], baseIdx)
				sel.Push(tmp[1], baseIdx+1)
				sel.Push(tmp[2], baseIdx+2)
				sel.Push(tmp[3], baseIdx+3)
				i += 4
				continue
			}
			idx := indices[offset+i]
			if idx < 0 {
				i++
				continue
			}
			if j := offset + i + batchPrefetchDistance; j < len(indices) {
				nextIdx := indices[j]
				if nextIdx >= 0 {
					next := unsafe.Add(vectors, uintptr(nextIdx)*uintptr(rowStride))
					Prefetch(next, 0)
				}
			}
			row := unsafe.Add(vectors, uintptr(idx)*uintptr(rowStride))
			sel.Push(L2SqRaw(qptr, row, dim), idx)
			i++
		}
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
