//go:build unix

package vpage

import (
	"syscall"
)

func mmapHuge(size int) ([]byte, error) {
	data, err := syscall.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON,
	)
	if err != nil {
		data, err = syscall.Mmap(-1, 0, size,
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS,
		)
	}
	return data, err
}

func mmapFileSyscall(fd uintptr, size int) ([]byte, error) {
	return syscall.Mmap(int(fd), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}

func munmapFileSyscall(data []byte) error {
	return syscall.Munmap(data)
}
