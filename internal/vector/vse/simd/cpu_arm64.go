//go:build arm64

package simd

import "golang.org/x/sys/cpu"

func detectCPU() {
	RuntimeInfo = Runtime{
		HasNEON:       cpu.ARM64.HasASIMD,
		CacheLineSize: 64,
		PageSize:      4096,
		HugePageSize:  2 << 20,
	}

	if RuntimeInfo.HasNEON {
		RuntimeInfo.MaxISA = ISANEON
	} else {
		RuntimeInfo.MaxISA = ISAGeneric
	}
}
