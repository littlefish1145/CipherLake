//go:build !amd64

package simd

import "unsafe"

func prefetchL0(addr unsafe.Pointer)  {}
func prefetchL1(addr unsafe.Pointer)  {}
func prefetchL2(addr unsafe.Pointer)  {}
func prefetchNTA(addr unsafe.Pointer) {}
