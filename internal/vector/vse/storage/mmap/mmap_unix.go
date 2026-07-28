//go:build !windows

package mmap

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

type Reader struct {
	data []byte
	file *os.File
	path string
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
	data, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	r := &Reader{data: data, file: f, path: path}
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
	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap create map %s: %w", path, err)
	}
	r := &Reader{data: data, file: f, path: path}
	runtime.SetFinalizer(r, (*Reader).Close)
	return r, nil
}

func (r *Reader) Data() []byte { return r.data }
func (r *Reader) Len() int     { return len(r.data) }
func (r *Reader) Path() string { return r.path }

func (r *Reader) Close() error {
	runtime.SetFinalizer(r, nil)
	var err1 error
	if r.data != nil {
		err1 = unix.Munmap(r.data)
		r.data = nil
	}
	var err2 error
	if r.file != nil {
		err2 = r.file.Close()
		r.file = nil
	}
	if err1 != nil {
		return err1
	}
	return err2
}

func (r *Reader) Sync() error {
	if r.data != nil {
		return unix.Msync(r.data, unix.MS_SYNC)
	}
	return nil
}

func (r *Reader) Resize(newSize int) error {
	if err := unix.Munmap(r.data); err != nil {
		return fmt.Errorf("munmap before resize: %w", err)
	}
	r.data = nil
	if err := r.file.Truncate(int64(newSize)); err != nil {
		return fmt.Errorf("truncate for resize: %w", err)
	}
	data, err := unix.Mmap(int(r.file.Fd()), 0, newSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap after resize: %w", err)
	}
	r.data = data
	return nil
}
