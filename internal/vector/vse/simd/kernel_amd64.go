//go:build amd64

package simd

import "unsafe"

//go:noescape
func dotAVX2(aptr, bptr unsafe.Pointer, n int) float32

//go:noescape
func dotSSE4(aptr, bptr unsafe.Pointer, n int) float32

//go:noescape
func l2SqAVX2(aptr, bptr unsafe.Pointer, n int) float32

//go:noescape
func l2SqSSE4(aptr, bptr unsafe.Pointer, n int) float32

func dotAVX2Wrap(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	return dotAVX2(unsafe.Pointer(&a[0]), unsafe.Pointer(&b[0]), len(a))
}

func dotSSE4Wrap(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	return dotSSE4(unsafe.Pointer(&a[0]), unsafe.Pointer(&b[0]), len(a))
}

func l2SqAVX2Wrap(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	return l2SqAVX2(unsafe.Pointer(&a[0]), unsafe.Pointer(&b[0]), len(a))
}

func l2SqSSE4Wrap(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	return l2SqSSE4(unsafe.Pointer(&a[0]), unsafe.Pointer(&b[0]), len(a))
}

func dotAVX512Wrap(a, b []float32) float32 {
	return dotAVX2Wrap(a, b)
}

func l2SqAVX512Wrap(a, b []float32) float32 {
	return l2SqAVX2Wrap(a, b)
}

var _ = dotAVX512Wrap
var _ = l2SqAVX512Wrap
