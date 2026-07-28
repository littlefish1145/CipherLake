package simd

import (
	"math"
	"sync"
	"unsafe"
)

type ISA int

const (
	ISAGeneric ISA = iota
	ISASSE4
	ISAAVX2
	ISAAVX512
	ISAVNNI
	ISAAMX
	ISANEON
)

type Runtime struct {
	MaxISA        ISA
	HasAVX512     bool
	HasAVX2       bool
	HasFMA        bool
	HasSSE42      bool
	HasVNNI       bool
	HasAMX        bool
	HasNEON       bool
	CacheLineSize int
	PageSize      int
	HugePageSize  int
	NumCPU        int
}

var RuntimeInfo Runtime
var initOnce sync.Once

func Init() {
	initOnce.Do(func() {
		detectCPU()
		initDispatch()
	})
}

func (r *Runtime) String() string {
	switch r.MaxISA {
	case ISAAVX512:
		return "AVX-512"
	case ISAAVX2:
		if RuntimeInfo.HasFMA {
			return "AVX2+FMA"
		}
		return "AVX2"
	case ISASSE4:
		return "SSE4.2"
	case ISAVNNI:
		return "VNNI"
	case ISAAMX:
		return "AMX"
	case ISANEON:
		return "NEON"
	default:
		return "Generic"
	}
}

var (
	Dot     func(a, b []float32) float32
	L2      func(a, b []float32) float32
	Cosine  func(a, b []float32) float32
	L2Sq    func(a, b []float32) float32
	DotRaw  func(a, b unsafe.Pointer, n int) float32
	L2SqRaw func(a, b unsafe.Pointer, n int) float32

	L2SqBatch4ContigRaw func(query, vectors unsafe.Pointer, dim int, out unsafe.Pointer)
	L2SqBatch4StrideRaw func(query, vectors unsafe.Pointer, dim, stride int, out unsafe.Pointer)
)

func initDispatch() {
	switch RuntimeInfo.MaxISA {
	case ISAAVX512:
		if RuntimeInfo.HasFMA {
			Dot = dotFMAWrap
			L2Sq = l2SqFMAWrap
			DotRaw = dotFMA
			L2SqRaw = l2SqFMA
			L2SqBatch4ContigRaw = l2SqBatch4ContigFMAWrap
			L2SqBatch4StrideRaw = l2SqBatch4StrideFMAWrap
		} else {
			Dot = dotAVX512Wrap
			L2Sq = l2SqAVX512Wrap
			DotRaw = dotAVX2
			L2SqRaw = l2SqAVX2
			L2SqBatch4ContigRaw = l2SqBatch4ContigAVX2Wrap
			L2SqBatch4StrideRaw = l2SqBatch4StrideAVX2Wrap
		}
	case ISAAVX2:
		if RuntimeInfo.HasFMA {
			Dot = dotFMAWrap
			L2Sq = l2SqFMAWrap
			DotRaw = dotFMA
			L2SqRaw = l2SqFMA
			L2SqBatch4ContigRaw = l2SqBatch4ContigFMAWrap
			L2SqBatch4StrideRaw = l2SqBatch4StrideFMAWrap
		} else {
			Dot = dotAVX2Wrap
			L2Sq = l2SqAVX2Wrap
			DotRaw = dotAVX2
			L2SqRaw = l2SqAVX2
			L2SqBatch4ContigRaw = l2SqBatch4ContigAVX2Wrap
			L2SqBatch4StrideRaw = l2SqBatch4StrideAVX2Wrap
		}
	case ISASSE4:
		Dot = dotSSE4Wrap
		L2Sq = l2SqSSE4Wrap
		DotRaw = dotSSE4
		L2SqRaw = l2SqSSE4
		L2SqBatch4ContigRaw = l2SqBatch4ContigFallback
		L2SqBatch4StrideRaw = l2SqBatch4StrideFallback
	case ISANEON:
		Dot = dotNEONWrap
		L2Sq = l2SqNEONWrap
		DotRaw = dotGenericRaw
		L2SqRaw = l2SqGenericRaw
		L2SqBatch4ContigRaw = l2SqBatch4ContigFallback
		L2SqBatch4StrideRaw = l2SqBatch4StrideFallback
	default:
		Dot = dotGenericWrap
		L2Sq = l2SqGenericWrap
		DotRaw = dotGenericRaw
		L2SqRaw = l2SqGenericRaw
		L2SqBatch4ContigRaw = l2SqBatch4ContigFallback
		L2SqBatch4StrideRaw = l2SqBatch4StrideFallback
	}

	L2 = func(a, b []float32) float32 {
		return float32(math.Sqrt(float64(L2Sq(a, b))))
	}
	Cosine = func(a, b []float32) float32 {
		return 1.0 - Dot(a, b)
	}
}

func dotGenericWrap(a, b []float32) float32  { return dotGeneric(a, b) }
func l2SqGenericWrap(a, b []float32) float32 { return l2SqGeneric(a, b) }
func dotNEONWrap(a, b []float32) float32     { return dotNEON(a, b) }
func l2SqNEONWrap(a, b []float32) float32    { return l2SqNEON(a, b) }
