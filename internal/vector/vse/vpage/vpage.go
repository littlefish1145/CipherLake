package vpage

import (
	"fmt"
	"os"
	"unsafe"

	"nexus/internal/vector/vse/simd"
	"golang.org/x/sys/cpu"
)

type Layout int8

const (
	LayoutAoS Layout = iota
	LayoutSoA
)

type VectorPage struct {
	base     unsafe.Pointer
	basePtr  unsafe.Pointer
	dim      int
	count    int
	capacity int
	layout   Layout
	stride   int
	mmapFile *os.File
	mmapData []byte
}

const (
	AlignHugePage int = 2 << 20
	AlignPage     int = 4096
)

func NewSoA(dim, capacity int) *VectorPage {
	if dim <= 0 || capacity <= 0 {
		panic("vpage: dim and capacity must be > 0")
	}

	totalBytes := dim * capacity * 4
	var ptr unsafe.Pointer

	if cpu.X86.HasAVX2 || cpu.ARM64.HasASIMD {
		ptr = allocHugePage(totalBytes)
	}
	if ptr == nil {
		ptr = allocPageAligned(totalBytes)
	}

	return &VectorPage{
		base:     ptr,
		dim:      dim,
		count:    0,
		capacity: capacity,
		layout:   LayoutSoA,
		stride:   capacity * 4,
	}
}

func NewAoS(dim, capacity int) *VectorPage {
	totalBytes := capacity * dim * 4
	var ptr unsafe.Pointer
	if cpu.X86.HasAVX2 || cpu.ARM64.HasASIMD {
		ptr = allocHugePage(totalBytes)
	}
	if ptr == nil {
		ptr = allocPageAligned(totalBytes)
	}
	return &VectorPage{
		base:     ptr,
		dim:      dim,
		count:    0,
		capacity: capacity,
		layout:   LayoutAoS,
		stride:   dim * 4,
	}
}

func MmapSoA(file *os.File, dim, count int) (*VectorPage, error) {
	totalBytes := dim * count * 4
	data, err := mmapFile(file, totalBytes)
	if err != nil {
		return nil, err
	}
	return &VectorPage{
		base:     unsafe.Pointer(&data[0]),
		dim:      dim,
		count:    count,
		capacity: count,
		layout:   LayoutSoA,
		stride:   count * 4,
		mmapFile: file,
		mmapData: data,
	}, nil
}

func MmapAoS(file *os.File, dim, count int) (*VectorPage, error) {
	totalBytes := count * dim * 4
	data, err := mmapFile(file, totalBytes)
	if err != nil {
		return nil, err
	}
	return &VectorPage{
		base:     unsafe.Pointer(&data[0]),
		dim:      dim,
		count:    count,
		capacity: count,
		layout:   LayoutAoS,
		stride:   dim * 4,
		mmapFile: file,
		mmapData: data,
	}, nil
}

func (vp *VectorPage) Dim() int    { return vp.dim }
func (vp *VectorPage) Count() int  { return vp.count }
func (vp *VectorPage) Cap() int    { return vp.capacity }
func (vp *VectorPage) Base() unsafe.Pointer { return vp.base }

func (vp *VectorPage) Close() error {
	if vp.mmapData != nil {
		if err := munmapFile(vp.mmapData); err != nil {
			return err
		}
		vp.mmapData = nil
	}
	if vp.mmapFile != nil {
		vp.mmapFile.Close()
		vp.mmapFile = nil
	}
	vp.base = nil
	return nil
}

func (vp *VectorPage) Append(vec []float32) error {
	if vp.count >= vp.capacity {
		return fmt.Errorf("vpage: page full (%d/%d)", vp.count, vp.capacity)
	}
	if len(vec) != vp.dim {
		return fmt.Errorf("vpage: dim mismatch %d != %d", len(vec), vp.dim)
	}

	if vp.layout == LayoutSoA {
		for d := 0; d < vp.dim; d++ {
			col := unsafe.Add(vp.base, uintptr(d)*uintptr(vp.stride))
			offset := uintptr(vp.count) * 4
			*(*float32)(unsafe.Add(col, offset)) = vec[d]
		}
	} else {
		dst := unsafe.Add(vp.base, uintptr(vp.count)*uintptr(vp.stride))
		copy(unsafe.Slice((*float32)(dst), vp.dim), vec)
	}
	vp.count++
	return nil
}

func (vp *VectorPage) At(idx int) []float32 {
	if idx < 0 || idx >= vp.count {
		return nil
	}
	if vp.layout == LayoutSoA {
		out := make([]float32, vp.dim)
		for d := 0; d < vp.dim; d++ {
			col := unsafe.Add(vp.base, uintptr(d)*uintptr(vp.stride))
			offset := uintptr(idx) * 4
			out[d] = *(*float32)(unsafe.Add(col, offset))
		}
		return out
	}
	ptr := unsafe.Add(vp.base, uintptr(idx)*uintptr(vp.stride))
	return unsafe.Slice((*float32)(ptr), vp.dim)
}

// AtInto 将 idx 处的向量拷贝到 dst，避免 heap 分配。
// 返回 dst 切片（可能被截断到 dim 长度）。dst 长度必须 >= dim。
// 对于 AoS 布局，直接返回对 mmap 内存的引用（零拷贝）。
func (vp *VectorPage) AtInto(idx int, dst []float32) []float32 {
	if idx < 0 || idx >= vp.count || len(dst) < vp.dim {
		return nil
	}
	if vp.layout == LayoutSoA {
		for d := 0; d < vp.dim; d++ {
			col := unsafe.Add(vp.base, uintptr(d)*uintptr(vp.stride))
			offset := uintptr(idx) * 4
			dst[d] = *(*float32)(unsafe.Add(col, offset))
		}
		return dst[:vp.dim]
	}
	ptr := unsafe.Add(vp.base, uintptr(idx)*uintptr(vp.stride))
	return unsafe.Slice((*float32)(ptr), vp.dim)
}

func (vp *VectorPage) AtPtr(idx int) unsafe.Pointer {
	if idx < 0 || idx >= vp.count {
		return nil
	}
	if vp.layout == LayoutSoA {
		return nil
	}
	return unsafe.Add(vp.base, uintptr(idx)*uintptr(vp.stride))
}

func (vp *VectorPage) GetDimSlice(dimIdx int) []float32 {
	if dimIdx < 0 || dimIdx >= vp.dim {
		return nil
	}
	if vp.layout != LayoutSoA {
		return nil
	}
	col := unsafe.Add(vp.base, uintptr(dimIdx)*uintptr(vp.stride))
	return unsafe.Slice((*float32)(col), vp.count)
}

func (vp *VectorPage) BatchDot(query []float32, results []float32, topK int) ([]float32, []int32) {
	if len(results) < vp.count {
		results = make([]float32, vp.count)
	}

	if vp.layout == LayoutSoA {
		vp.batchDotSoA(query, results)
	} else {
		simd.BatchDot(query, vp.base, vp.dim, vp.count, results)
	}

	return simd.TopK(results, topK)
}

func (vp *VectorPage) BatchL2Sq(query []float32, results []float32, topK int) ([]float32, []int32) {
	if len(results) < vp.count {
		results = make([]float32, vp.count)
	}

	if vp.layout == LayoutSoA {
		vp.batchL2SqSoA(query, results)
	} else {
		simd.BatchL2Sq(query, vp.base, vp.dim, vp.count, results)
	}

	return simd.TopK(results, topK)
}

func (vp *VectorPage) batchDotSoA(query []float32, results []float32) {
	for i := range results {
		results[i] = 0
	}
	for d := 0; d < vp.dim; d++ {
		qd := query[d]
		col := unsafe.Add(vp.base, uintptr(d)*uintptr(vp.stride))
		for i := 0; i < vp.count; i++ {
			vd := *(*float32)(unsafe.Add(col, uintptr(i)*4))
			results[i] += vd * qd
		}
	}
}

func (vp *VectorPage) batchL2SqSoA(query []float32, results []float32) {
	for i := range results {
		results[i] = 0
	}
	for d := 0; d < vp.dim; d++ {
		qd := query[d]
		col := unsafe.Add(vp.base, uintptr(d)*uintptr(vp.stride))
		for i := 0; i < vp.count; i++ {
			vd := *(*float32)(unsafe.Add(col, uintptr(i)*4))
			diff := vd - qd
			results[i] += diff * diff
		}
	}
}

func (vp *VectorPage) Prefetch(level int) {
	if vp.base == nil {
		return
	}
	total := vp.dim * vp.capacity * 4
	ptr := uintptr(vp.base)
	end := ptr + uintptr(total)
	for ptr < end {
		simd.Prefetch(unsafe.Pointer(ptr), level)
		ptr += 64
	}
}

func allocHugePage(size int) unsafe.Pointer {
	size = alignUp(size, AlignHugePage)

	data, err := mmapHuge(size)
	if err == nil && data != nil {
		return unsafe.Pointer(&data[0])
	}
	return nil
}

func allocPageAligned(size int) unsafe.Pointer {
	size = alignUp(size, AlignPage)
	data := make([]byte, size+AlignPage)
	ptr := unsafe.Pointer(&data[0])
	aligned := alignUp(int(uintptr(ptr)), AlignPage)
	offset := aligned - int(uintptr(ptr))
	if offset == 0 {
		return ptr
	}
	return unsafe.Pointer(uintptr(ptr) + uintptr(offset))
}

func alignUp(n, align int) int {
	return (n + align - 1) & ^(align - 1)
}

func init() {
	simd.Init()
}

func mmapFile(file *os.File, size int) ([]byte, error) {
	return mmapFileSyscall(file.Fd(), size)
}

func munmapFile(data []byte) error {
	return munmapFileSyscall(data)
}
