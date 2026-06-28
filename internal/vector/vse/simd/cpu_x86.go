//go:build amd64

package simd

import "golang.org/x/sys/cpu"

func detectCPU() {
	RuntimeInfo = Runtime{
		HasAVX512: cpu.X86.HasAVX512F,
		HasAVX2:   cpu.X86.HasAVX2,
		HasSSE42:  cpu.X86.HasSSE42,
		HasVNNI:   cpu.X86.HasAVX512VNNI,
		HasAMX:    cpu.X86.HasAMXTile,
		CacheLineSize: 64,
		PageSize:      4096,
		HugePageSize:  2 << 20,
	}

	switch {
	case RuntimeInfo.HasAVX512:
		RuntimeInfo.MaxISA = ISAAVX512
	case RuntimeInfo.HasAVX2:
		RuntimeInfo.MaxISA = ISAAVX2
	case RuntimeInfo.HasSSE42:
		RuntimeInfo.MaxISA = ISASSE4
	default:
		RuntimeInfo.MaxISA = ISAGeneric
	}
}
