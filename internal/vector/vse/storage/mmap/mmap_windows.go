//go:build windows

package mmap

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type Reader struct {
	data   []byte
	file   *os.File
	path   string
	handle windows.Handle
	ptr    uintptr
}

//go:nocheckptr
func ptrToSlice(ptr uintptr, size int) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size)
}

func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mmap open %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap stat %s: %w", path, err)
	}
	if fi.Size() == 0 {
		f.Close()
		return nil, fmt.Errorf("mmap %s: empty file", path)
	}

	size := fi.Size()
	lowSize := uint32(size & 0xFFFFFFFF)
	highSize := uint32(size >> 32)

	hdl, err := windows.CreateFileMapping(
		windows.Handle(f.Fd()), nil, windows.PAGE_READONLY,
		highSize, lowSize, nil,
	)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap CreateFileMapping %s: %w", path, err)
	}

	ptr, err := windows.MapViewOfFile(hdl, windows.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(hdl)
		f.Close()
		return nil, fmt.Errorf("mmap MapViewOfFile %s: %w", path, err)
	}

	r := &Reader{file: f, path: path, handle: hdl, ptr: ptr}
	r.data = ptrToSlice(ptr, int(size))
	runtime.SetFinalizer(r, (*Reader).Close)
	return r, nil
}

func Create(path string, size int) (*Reader, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("mmap create %s: %w", path, err)
	}
	if size > 0 {
		if err := f.Truncate(int64(size)); err != nil {
			f.Close()
			return nil, fmt.Errorf("mmap truncate %s: %w", path, err)
		}
	}

	lowSize := uint32(size & 0xFFFFFFFF)
	highSize := uint32(size >> 32)

	hdl, err := windows.CreateFileMapping(
		windows.Handle(f.Fd()), nil, windows.PAGE_READWRITE,
		highSize, lowSize, nil,
	)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap CreateFileMapping %s: %w", path, err)
	}

	ptr, err := windows.MapViewOfFile(hdl, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(hdl)
		f.Close()
		return nil, fmt.Errorf("mmap MapViewOfFile %s: %w", path, err)
	}

	r := &Reader{file: f, path: path, handle: hdl, ptr: ptr}
	r.data = ptrToSlice(ptr, size)
	runtime.SetFinalizer(r, (*Reader).Close)
	return r, nil
}

func (r *Reader) Data() []byte { return r.data }
func (r *Reader) Len() int     { return len(r.data) }
func (r *Reader) Path() string { return r.path }

func (r *Reader) Close() error {
	runtime.SetFinalizer(r, nil)
	r.data = nil
	if r.ptr != 0 {
		windows.UnmapViewOfFile(r.ptr)
		r.ptr = 0
	}
	if r.handle != 0 {
		windows.CloseHandle(r.handle)
		r.handle = 0
	}
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
	return nil
}

func (r *Reader) Sync() error {
	if r.ptr != 0 {
		return windows.FlushViewOfFile(r.ptr, uintptr(len(r.data)))
	}
	return nil
}

func (r *Reader) Resize(newSize int) error {
	if r.ptr != 0 {
		windows.UnmapViewOfFile(r.ptr)
		r.ptr = 0
	}
	r.data = nil
	if r.handle != 0 {
		windows.CloseHandle(r.handle)
		r.handle = 0
	}
	if err := r.file.Truncate(int64(newSize)); err != nil {
		return fmt.Errorf("truncate for resize: %w", err)
	}

	lowSize := uint32(newSize & 0xFFFFFFFF)
	highSize := uint32(newSize >> 32)

	hdl, err := windows.CreateFileMapping(
		windows.Handle(r.file.Fd()), nil, windows.PAGE_READWRITE,
		highSize, lowSize, nil,
	)
	if err != nil {
		return fmt.Errorf("mmap CreateFileMapping resize: %w", err)
	}

	ptr, err := windows.MapViewOfFile(hdl, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, uintptr(newSize))
	if err != nil {
		windows.CloseHandle(hdl)
		return fmt.Errorf("mmap MapViewOfFile resize: %w", err)
	}

	r.handle = hdl
	r.ptr = ptr
	r.data = ptrToSlice(ptr, newSize)
	return nil
}
