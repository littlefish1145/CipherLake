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
)

func initDispatch() {
	switch RuntimeInfo.MaxISA {
	case ISAAVX512:
		Dot = dotAVX512Wrap
		L2Sq = l2SqAVX512Wrap
	case ISAAVX2:
		Dot = dotAVX2Wrap
		L2Sq = l2SqAVX2Wrap
	case ISASSE4:
		Dot = dotSSE4Wrap
		L2Sq = l2SqSSE4Wrap
	case ISANEON:
		Dot = dotNEONWrap
		L2Sq = l2SqNEONWrap
	default:
		Dot = dotGenericWrap
		L2Sq = l2SqGenericWrap
	}

	L2 = func(a, b []float32) float32 {
		return float32(math.Sqrt(float64(L2Sq(a, b))))
	}
	Cosine = func(a, b []float32) float32 {
		return 1.0 - Dot(a, b)
	}
	DotRaw = func(a, b unsafe.Pointer, n int) float32 {
		sa := unsafe.Slice((*float32)(a), n)
		sb := unsafe.Slice((*float32)(b), n)
		return Dot(sa, sb)
	}
	L2SqRaw = func(a, b unsafe.Pointer, n int) float32 {
		sa := unsafe.Slice((*float32)(a), n)
		sb := unsafe.Slice((*float32)(b), n)
		return L2Sq(sa, sb)
	}
}

func dotGenericWrap(a, b []float32) float32 { return dotGeneric(a, b) }
func l2SqGenericWrap(a, b []float32) float32  { return l2SqGeneric(a, b) }
func dotNEONWrap(a, b []float32) float32 { return dotNEON(a, b) }
func l2SqNEONWrap(a, b []float32) float32  { return l2SqNEON(a, b) }
