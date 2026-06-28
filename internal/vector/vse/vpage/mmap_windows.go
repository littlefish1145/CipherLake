//go:build windows

package vpage

import (
	"syscall"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc  = kernel32.NewProc("VirtualAlloc")
	procCreateFileMappingW = kernel32.NewProc("CreateFileMappingW")
	procMapViewOfFile     = kernel32.NewProc("MapViewOfFile")
	procUnmapViewOfFile   = kernel32.NewProc("UnmapViewOfFile")

	MEM_COMMIT       uintptr = 0x1000
	MEM_RESERVE      uintptr = 0x2000
	MEM_LARGE_PAGES  uintptr = 0x20000000
	PAGE_READWRITE   uintptr = 0x04
	FILE_MAP_ALL_ACCESS uintptr = 0xF001F
)

func mmapHuge(size int) ([]byte, error) {
	base, _, err := procVirtualAlloc.Call(
		0,
		uintptr(size),
		uintptr(MEM_RESERVE|MEM_COMMIT|MEM_LARGE_PAGES),
		PAGE_READWRITE,
	)
	if base == 0 {
		return nil, err
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(base)), size), nil
}

func mmapFileSyscall(fileFd uintptr, size int) ([]byte, error) {
	hMap, _, err := procCreateFileMappingW.Call(
		fileFd,
		0,
		PAGE_READWRITE,
		0,
		uintptr(size),
		0,
	)
	if hMap == 0 {
		return nil, err
	}

	ptr, _, err := procMapViewOfFile.Call(
		hMap,
		FILE_MAP_ALL_ACCESS,
		0,
		0,
		uintptr(size),
	)
	if ptr == 0 {
		return nil, err
	}

	return unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size), nil
}

func munmapFileSyscall(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	ptr := uintptr(unsafe.Pointer(&data[0]))
	r, _, _ := procUnmapViewOfFile.Call(ptr)
	if r == 0 {
		return syscall.EINVAL
	}
	return nil
}
