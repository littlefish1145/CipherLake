//go:build !amd64 && !arm64

package simd

func detectCPU() {
	RuntimeInfo = Runtime{
		CacheLineSize: 64,
		PageSize:      4096,
		HugePageSize:  2 << 20,
	}
	RuntimeInfo.MaxISA = ISAGeneric
}
