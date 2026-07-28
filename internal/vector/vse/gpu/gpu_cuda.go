//go:build cuda

package gpu

/*
#cgo LDFLAGS: -lcuda -lcublas
#cgo CFLAGS:

#include <cuda.h>
#include <cublas_v2.h>

// Helper: launch a CUDA kernel with args as void** array
CUresult launchKernel(CUfunction f,
    unsigned int gx, unsigned int gy, unsigned int gz,
    unsigned int bx, unsigned int by, unsigned int bz,
    unsigned int smem, CUstream s,
    void* args) {
    return cuLaunchKernel(f, gx, gy, gz, bx, by, bz, smem, s, (void**)args, NULL);
}

// Async memcpy helpers
CUresult memcpyH2DAsync(CUdeviceptr dst, const void* src, size_t bytes, CUstream s) {
    return cuMemcpyHtoDAsync(dst, src, bytes, s);
}
CUresult memcpyD2HAsync(void* dst, CUdeviceptr src, size_t bytes, CUstream s) {
    return cuMemcpyDtoHAsync(dst, src, bytes, s);
}
*/
import "C"
import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"unsafe"

	"cipherlake/internal/vector/vse"
	"cipherlake/internal/vector/vse/engine/quantizer"
)

// ---------------------------------------------------------------------------
// GPU segment data
// ---------------------------------------------------------------------------

type gpuSegData struct {
	dCodes                 unsafe.Pointer // PQ codes (nv × M bytes)
	dCodebook              unsafe.Pointer // PQ codebook (M × 256 × subdim float32)
	dCentroids             unsafe.Pointer // centroids (nc × dim float32)
	dVectors               unsafe.Pointer // exact vectors (nv × dim float32), optional
	dNorms                 unsafe.Pointer // vec norms² (nv × float32), optional
	dGlobalIDs             unsafe.Pointer // global IDs (nv × uint64)
	nv, dim, nc, M, subdim int

	// GPU-side IVF lists (CSR format)
	dClusterOffsets unsafe.Pointer // [nc+1] int32
	dClusterMembers unsafe.Pointer // [total] int32, flattened vector indices

	// Vamana graph data (CSR format)
	dNbrOffsets unsafe.Pointer // [nv+1] int32
	dNbrData    unsafe.Pointer // [totalNbrs] int32

	// host-side cache for rerank
	hostGids  []uint64  // [nv], cached global IDs
	hostNorms []float32 // [nv], cached vector norms² (optional)

	// IVF cluster lists (CPU-side, for selectCandidates)
	clusterOffsets []int32 // [nc+1], offsets[c] = start index in clusterMembers for centroid c
	clusterMembers []int32 // flattened, size clusterOffsets[nc]; all vector indices sorted by centroid
}

// ---------------------------------------------------------------------------
// Compiled kernels
// ---------------------------------------------------------------------------

type cudaKernels struct {
	mod                   C.CUmodule
	pqDistTable           C.CUfunction
	pqADC                 C.CUfunction
	pqADCGather           C.CUfunction
	centroidDist          C.CUfunction
	computeNorms          C.CUfunction
	l2FromDot             C.CUfunction
	cosineFromDot         C.CUfunction
	topkSelect            C.CUfunction
	gatherDot             C.CUfunction
	gatherL2              C.CUfunction
	gatherCosine          C.CUfunction
	topkSelectGather      C.CUfunction
	topkSelectIndices     C.CUfunction
	gatherIVFCandidates   C.CUfunction
	batchedTopKSelectGids C.CUfunction
	mergeTopKPositions    C.CUfunction
	centroidSelectTopK    C.CUfunction
	vamanaExpand          C.CUfunction
	vamanaMergeFrontier   C.CUfunction
}

// ---------------------------------------------------------------------------
// managerImpl
// ---------------------------------------------------------------------------

type cudaManager struct {
	cfg      Config
	mu       sync.Mutex
	initDone bool
	handle   C.cublasHandle_t
	stream   C.CUstream
	dev      C.CUdevice
	ctx      C.CUcontext
	segments map[uint32]*gpuSegData
	kernels  *cudaKernels

	// 查询缓冲池：复用 dQ/dDists/dTable 避免每次搜索 malloc/free
	queryPool *queryBufPool
}

// queryBufPool 复用 GPU 内存缓冲区
type queryBufPool struct {
	dQ         unsafe.Pointer // [maxDim] float32
	dDists     unsafe.Pointer // [maxNV] float32
	dTable     unsafe.Pointer // [maxM*256] float32
	dCDists    unsafe.Pointer // [maxNC] float32
	dRerankIdx unsafe.Pointer // [maxRerank] int32, rerank 候选索引
	dRerankD   unsafe.Pointer // [maxRerank] float32, rerank 精确距离
	maxDim     int
	maxNV      int
	maxM       int
	maxNC      int
	maxRerank  int
}

func newQueryBufPool() *queryBufPool {
	return &queryBufPool{}
}

// ensureCapacity 确保缓冲区足够大，不够则重新分配
func (p *queryBufPool) ensureCapacity(dim, nv, m, nc int) error {
	if p.maxDim < dim {
		if p.dQ != nil {
			C.cuMemFree(C.CUdeviceptr(uintptr(p.dQ)))
		}
		if err := cuMalloc(&p.dQ, dim*4); err != nil {
			return err
		}
		p.maxDim = dim
	}
	if p.maxNV < nv {
		if p.dDists != nil {
			C.cuMemFree(C.CUdeviceptr(uintptr(p.dDists)))
		}
		if err := cuMalloc(&p.dDists, nv*4); err != nil {
			return err
		}
		p.maxNV = nv
	}
	if p.maxM < m {
		if p.dTable != nil {
			C.cuMemFree(C.CUdeviceptr(uintptr(p.dTable)))
		}
		if err := cuMalloc(&p.dTable, m*256*4); err != nil {
			return err
		}
		p.maxM = m
	}
	if p.maxNC < nc {
		if p.dCDists != nil {
			C.cuMemFree(C.CUdeviceptr(uintptr(p.dCDists)))
		}
		if err := cuMalloc(&p.dCDists, nc*4); err != nil {
			return err
		}
		p.maxNC = nc
	}
	return nil
}

// ensureRerankCapacity 确保 rerank 缓冲区足够大
func (p *queryBufPool) ensureRerankCapacity(rerankK int) error {
	if p.maxRerank < rerankK {
		if p.dRerankIdx != nil {
			C.cuMemFree(C.CUdeviceptr(uintptr(p.dRerankIdx)))
		}
		if err := cuMalloc(&p.dRerankIdx, rerankK*4); err != nil {
			return err
		}
		if p.dRerankD != nil {
			C.cuMemFree(C.CUdeviceptr(uintptr(p.dRerankD)))
		}
		if err := cuMalloc(&p.dRerankD, rerankK*4); err != nil {
			return err
		}
		p.maxRerank = rerankK
	}
	return nil
}

func (p *queryBufPool) free() {
	for _, ptr := range []unsafe.Pointer{p.dQ, p.dDists, p.dTable, p.dCDists, p.dRerankIdx, p.dRerankD} {
		if ptr != nil {
			C.cuMemFree(C.CUdeviceptr(uintptr(ptr)))
		}
	}
	p.dQ = nil
	p.dDists = nil
	p.dTable = nil
	p.dCDists = nil
	p.dRerankIdx = nil
	p.dRerankD = nil
	p.maxDim = 0
	p.maxNV = 0
	p.maxM = 0
	p.maxNC = 0
	p.maxRerank = 0
}

func newGPUImpl(cfg Config) gpuImpl {
	return &cudaManager{
		cfg:       cfg,
		segments:  make(map[uint32]*gpuSegData),
		queryPool: newQueryBufPool(),
	}
}

func (m *cudaManager) init() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initDone {
		return nil
	}

	if err := cuCheck(C.cuInit(0)); err != nil {
		return fmt.Errorf("cuInit: %w", err)
	}
	var count C.int
	if err := cuCheck(C.cuDeviceGetCount(&count)); err != nil {
		return fmt.Errorf("cuDeviceGetCount: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("no CUDA device")
	}
	var dev C.CUdevice
	if err := cuCheck(C.cuDeviceGet(&dev, 0)); err != nil {
		return fmt.Errorf("cuDeviceGet: %w", err)
	}
	m.dev = dev
	// Retain primary context so cuMemAlloc etc. work
	if err := cuCheck(C.cuDevicePrimaryCtxRetain(&m.ctx, dev)); err != nil {
		return fmt.Errorf("cuDevicePrimaryCtxRetain: %w", err)
	}
	if err := cuCheck(C.cuCtxSetCurrent(m.ctx)); err != nil {
		return fmt.Errorf("cuCtxSetCurrent: %w", err)
	}
	if err := cuBlasCheck(C.cublasCreate(&m.handle)); err != nil {
		return fmt.Errorf("cublasCreate: %w", err)
	}
	if err := cuCheck(C.cuStreamCreate(&m.stream, C.CU_STREAM_DEFAULT)); err != nil {
		return fmt.Errorf("cuStreamCreate: %w", err)
	}
	// Associate cuBLAS with our stream
	C.cublasSetStream(m.handle, m.stream)

	kn, err := loadKernels(m.cfg.PTXPath)
	if err != nil {
		return fmt.Errorf("load kernels: %w", err)
	}
	m.kernels = kn

	m.initDone = true
	return nil
}

func (m *cudaManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.initDone {
		return
	}
	for id, d := range m.segments {
		m.freeSegData(d)
		delete(m.segments, id)
	}
	if m.queryPool != nil {
		m.queryPool.free()
	}
	if m.kernels != nil {
		C.cuModuleUnload(m.kernels.mod)
	}
	if m.stream != nil {
		C.cuStreamDestroy(m.stream)
	}
	if m.handle != nil {
		C.cublasDestroy(m.handle)
	}
	if m.ctx != nil {
		C.cuDevicePrimaryCtxRelease(m.dev)
	}
	m.initDone = false
}

func (m *cudaManager) enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initDone
}

// cudaLock locks the OS thread and sets the CUDA context current.
// Must be called while holding m.mu. Returns a function to unlock the thread.
func (m *cudaManager) cudaLock() func() {
	runtime.LockOSThread()
	if m.ctx != nil {
		C.cuCtxSetCurrent(m.ctx)
	}
	return runtime.UnlockOSThread
}

// ---------------------------------------------------------------------------
// Load pre-compiled PTX from file (replaces NVRTC runtime compilation)
// ---------------------------------------------------------------------------

func loadKernels(ptxPath string) (*cudaKernels, error) {
	// Resolve PTX path: try config path, then default "kernels.ptx", then alongside executable
	candidates := []string{ptxPath, "kernels.ptx"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates,
			filepath.Join(filepath.Dir(exe), "kernels.ptx"),
			filepath.Join(filepath.Dir(exe), "gpu", "kernels.ptx"),
		)
	}

	var found string
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			found = p
			break
		}
	}
	if found == "" {
		return nil, fmt.Errorf("PTX file not found (tried: %v). Compile kernels.cu with: nvcc -ptx -arch=compute_75 kernels.cu -o kernels.ptx", candidates)
	}

	cPath := C.CString(found)
	defer C.free(unsafe.Pointer(cPath))

	var mod C.CUmodule
	if err := cuCheck(C.cuModuleLoad(&mod, cPath)); err != nil {
		return nil, fmt.Errorf("cuModuleLoad(%s): %w", found, err)
	}

	kn := &cudaKernels{mod: mod}
	for _, f := range []struct {
		name string
		dst  *C.CUfunction
	}{
		{"pq_dist_table", &kn.pqDistTable},
		{"pq_adc", &kn.pqADC},
		{"pq_adc_gather", &kn.pqADCGather},
		{"centroid_dist", &kn.centroidDist},
		{"compute_norms", &kn.computeNorms},
		{"l2_from_dot", &kn.l2FromDot},
		{"cosine_from_dot", &kn.cosineFromDot},
		{"topk_select", &kn.topkSelect},
		{"gather_dot", &kn.gatherDot},
		{"gather_l2", &kn.gatherL2},
		{"gather_cosine", &kn.gatherCosine},
		{"topk_select_gather", &kn.topkSelectGather},
		{"topk_select_indices", &kn.topkSelectIndices},
		{"gather_ivf_candidates", &kn.gatherIVFCandidates},
		{"batched_topk_select_gids", &kn.batchedTopKSelectGids},
		{"merge_topk_positions", &kn.mergeTopKPositions},
		{"centroid_select_topk", &kn.centroidSelectTopK},
		{"vamana_expand", &kn.vamanaExpand},
		{"vamana_merge_frontier", &kn.vamanaMergeFrontier},
	} {
		cName := C.CString(f.name)
		if err := cuCheck(C.cuModuleGetFunction(f.dst, mod, cName)); err != nil {
			C.free(unsafe.Pointer(cName))
			return nil, fmt.Errorf("get function %s: %w", f.name, err)
		}
		C.free(unsafe.Pointer(cName))
	}
	return kn, nil
}

// ---------------------------------------------------------------------------
// Kernel launch helpers
// ---------------------------------------------------------------------------

// fastLaunch launches a CUDA kernel with args passed as void** array.
// Allocates arg memory on C heap to satisfy Go 1.25+ cgo pointer checker.
func (m *cudaManager) fastLaunch(fn C.CUfunction, gridX, gridY, blockX, blockY uint32, args ...unsafe.Pointer) error {
	return m.fastLaunchSmem(fn, gridX, gridY, blockX, blockY, 0, args...)
}

// fastLaunchSmem 同 fastLaunch 但支持指定 shared memory 大小
func (m *cudaManager) fastLaunchSmem(fn C.CUfunction, gridX, gridY, blockX, blockY, smem uint32, args ...unsafe.Pointer) error {
	n := len(args)
	if n == 0 {
		return cuCheck(C.launchKernel(fn,
			C.uint(gridX), C.uint(gridY), 1,
			C.uint(blockX), C.uint(blockY), 1,
			C.uint(smem), nil, nil,
		))
	}

	cArgs := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(uintptr(0))))
	argPtrs := unsafe.Slice((*unsafe.Pointer)(cArgs), n)
	for i, a := range args {
		cVal := C.malloc(8)
		*(*uint64)(cVal) = *(*uint64)(a)
		argPtrs[i] = cVal
	}

	err := cuCheck(C.launchKernel(fn,
		C.uint(gridX), C.uint(gridY), 1,
		C.uint(blockX), C.uint(blockY), 1,
		C.uint(smem), nil, cArgs,
	))

	for i := 0; i < n; i++ {
		C.free(argPtrs[i])
	}
	C.free(cArgs)
	return err
}

// argPtr returns a pointer to v that stays alive until the kernel completes.
// For device pointers (unsafe.Pointer), v is the pointer value itself.
func intArg(v *C.int) unsafe.Pointer         { return unsafe.Pointer(v) }
func uintArg(v *C.uint) unsafe.Pointer       { return unsafe.Pointer(v) }
func floatArg(v *C.float) unsafe.Pointer     { return unsafe.Pointer(v) }
func ptrArg(v unsafe.Pointer) unsafe.Pointer { return unsafe.Pointer(&v) } // pointer to pointer

// ---------------------------------------------------------------------------
// Pinned memory: upload segment data to GPU
// ---------------------------------------------------------------------------

func (m *cudaManager) pinSegment(meta *vse.SegmentMeta, vectors []byte,
	pq *quantizer.PQQuantizer, ivf *quantizer.IVF) error {

	m.mu.Lock()
	unlock := m.cudaLock()
	defer unlock()
	defer m.mu.Unlock()

	segID := uint32(meta.ID)
	if _, exists := m.segments[segID]; exists {
		return nil
	}

	nv, dim := meta.NumVectors, meta.Dimension
	M := pq.SubVecs()
	subd := pq.SubDim()
	nc := len(ivf.Centroids)

	if dim == 0 || nv == 0 || M == 0 || nc == 0 {
		return fmt.Errorf("invalid segment data: nv=%d dim=%d M=%d nc=%d", nv, dim, M, nc)
	}

	// --- Extract flat vectors + globalIDs from vectors.bin ---
	rowSize := 8 + dim*4
	flatVecs := make([]float32, nv*dim)
	gids := make([]uint64, nv)
	for i := 0; i < nv; i++ {
		off := 8 + i*rowSize
		gids[i] = binary.LittleEndian.Uint64(vectors[off:])
		for j := 0; j < dim; j++ {
			flatVecs[i*dim+j] = math.Float32frombits(binary.LittleEndian.Uint32(vectors[off+8+j*4:]))
		}
	}

	// --- PQ codes ---
	pqCodes := make([]byte, nv*M)
	for i := 0; i < nv; i++ {
		code, err := pq.Encode(flatVecs[i*dim : (i+1)*dim])
		if err != nil {
			return fmt.Errorf("encode vec %d: %w", i, err)
		}
		copy(pqCodes[i*M:(i+1)*M], code)
	}

	// --- PQ codebook: [M][256][subdim] → flat [M*256*subdim] ---
	centroids := pq.Centroids()
	cbFlat := make([]float32, M*256*subd)
	for mm := 0; mm < M; mm++ {
		for k := 0; k < 256 && k < len(centroids[mm]); k++ {
			copy(cbFlat[(mm*256+k)*subd:], centroids[mm][k][:subd])
		}
	}

	// --- Centroids: [nc][dim] → flat ---
	centFlat := make([]float32, nc*dim)
	for i := 0; i < nc; i++ {
		copy(centFlat[i*dim:], ivf.Centroids[i][:dim])
	}

	// --- GPU allocations ---
	d := &gpuSegData{nv: nv, dim: dim, nc: nc, M: M, subdim: subd}

	// Build cluster CSR from IVF lists
	d.clusterOffsets = make([]int32, nc+1)
	total := 0
	for c := 0; c < nc; c++ {
		d.clusterOffsets[c] = int32(total)
		total += len(ivf.Lists[c])
	}
	d.clusterOffsets[nc] = int32(total)
	d.clusterMembers = make([]int32, total)
	for c := 0; c < nc; c++ {
		start := d.clusterOffsets[c]
		for j, v := range ivf.Lists[c] {
			d.clusterMembers[int(start)+j] = int32(v)
		}
	}

	cleanup := func() { m.freeSegData(d) }

	alloc := func(p *unsafe.Pointer, sz int, name string) error {
		if err := cuMalloc(p, sz); err != nil {
			cleanup()
			return fmt.Errorf("malloc %s: %w", name, err)
		}
		return nil
	}

	upload := func(dst unsafe.Pointer, src unsafe.Pointer, sz int, name string) error {
		if err := cuMemcpyH2D(dst, src, sz); err != nil {
			cleanup()
			return fmt.Errorf("upload %s: %w", name, err)
		}
		return nil
	}

	// Upload IVF lists to GPU (CSR format)
	if err := alloc(&d.dClusterOffsets, (nc+1)*4, "clusterOffsets"); err != nil {
		return err
	}
	if err := upload(d.dClusterOffsets, unsafe.Pointer(&d.clusterOffsets[0]), (nc+1)*4, "clusterOffsets"); err != nil {
		return err
	}
	if total > 0 {
		if err := alloc(&d.dClusterMembers, total*4, "clusterMembers"); err != nil {
			return err
		}
		if err := upload(d.dClusterMembers, unsafe.Pointer(&d.clusterMembers[0]), total*4, "clusterMembers"); err != nil {
			return err
		}
	}

	if err := alloc(&d.dCodes, nv*M, "codes"); err != nil {
		return err
	}
	if err := alloc(&d.dCodebook, len(cbFlat)*4, "codebook"); err != nil {
		return err
	}
	if err := alloc(&d.dCentroids, len(centFlat)*4, "centroids"); err != nil {
		return err
	}
	if err := alloc(&d.dGlobalIDs, nv*8, "globalIDs"); err != nil {
		return err
	}

	if err := upload(d.dCodes, unsafe.Pointer(&pqCodes[0]), nv*M, "codes"); err != nil {
		return err
	}
	if err := upload(d.dCodebook, unsafe.Pointer(&cbFlat[0]), len(cbFlat)*4, "codebook"); err != nil {
		return err
	}
	if err := upload(d.dCentroids, unsafe.Pointer(&centFlat[0]), len(centFlat)*4, "centroids"); err != nil {
		return err
	}
	if err := upload(d.dGlobalIDs, unsafe.Pointer(&gids[0]), nv*8, "gids"); err != nil {
		return err
	}

	// Cache host-side copies for rerank (avoid repeated D2H transfers)
	d.hostGids = make([]uint64, nv)
	copy(d.hostGids, gids)

	// Optional: upload exact vectors for rerank
	if m.cfg.RerankCandidates > 0 {
		if err := alloc(&d.dVectors, nv*dim*4, "vectors"); err != nil {
			return err
		}
		if err := alloc(&d.dNorms, nv*4, "norms"); err != nil {
			return err
		}
		if err := upload(d.dVectors, unsafe.Pointer(&flatVecs[0]), nv*dim*4, "vectors"); err != nil {
			return err
		}
		// compute norms on GPU
		m.launchNorms(m.kernels.computeNorms, d.dVectors, d.dNorms, nv, dim)
		// cache norms on host for rerank
		d.hostNorms = make([]float32, nv)
		if err := cuMemcpyD2H(unsafe.Pointer(&d.hostNorms[0]), d.dNorms, nv*4); err != nil {
			return err
		}
	}

	m.segments[segID] = d
	return nil
}

func (m *cudaManager) unpinSegment(segID vse.SegmentID) error {
	m.mu.Lock()
	unlock := m.cudaLock()
	defer unlock()
	defer m.mu.Unlock()
	d, ok := m.segments[uint32(segID)]
	if !ok {
		return nil
	}
	m.freeSegData(d)
	delete(m.segments, uint32(segID))
	return nil
}

func (m *cudaManager) freeSegData(d *gpuSegData) {
	for _, p := range []unsafe.Pointer{d.dCodes, d.dCodebook, d.dCentroids, d.dVectors, d.dNorms, d.dGlobalIDs, d.dClusterOffsets, d.dClusterMembers, d.dNbrOffsets, d.dNbrData} {
		if p != nil {
			C.cuMemFree(C.CUdeviceptr(uintptr(p)))
		}
	}
}

// ---------------------------------------------------------------------------
// Search dispatch
// ---------------------------------------------------------------------------

func (m *cudaManager) search(segID vse.SegmentID, req SearchRequest) ([]vse.SearchResult, error) {
	m.mu.Lock()
	threadUnlock := m.cudaLock()
	d, ok := m.segments[uint32(segID)]
	m.mu.Unlock()
	if !ok {
		threadUnlock()
		return nil, fmt.Errorf("segment %d not on GPU", segID)
	}
	defer threadUnlock()
	if len(req.Query) != d.dim {
		return nil, fmt.Errorf("query dim %d != segment dim %d", len(req.Query), d.dim)
	}

	if req.Exact && d.dVectors != nil {
		return m.flatSearch(d, req.Query, req.TopK, req.Metric)
	}
	return m.pqSearch(d, req.Query, req.TopK, req.NProbe, req.Metric)
}

func (m *cudaManager) batchSearch(segID vse.SegmentID, reqs []SearchRequest) []BatchResult {
	if len(reqs) == 0 {
		return nil
	}
	m.mu.Lock()
	threadUnlock := m.cudaLock()
	d, ok := m.segments[uint32(segID)]
	m.mu.Unlock()
	if !ok {
		threadUnlock()
		results := make([]BatchResult, len(reqs))
		for i := range results {
			results[i].Err = fmt.Errorf("segment %d not on GPU", segID)
		}
		return results
	}
	defer threadUnlock()

	results := make([]BatchResult, len(reqs))

	if len(reqs) >= m.cfg.BatchThreshold {
		return m.batchPQSearch(d, reqs)
	}

	// fallthrough: per-query
	for i, req := range reqs {
		r, err := m.search(segID, req)
		results[i].Results, results[i].Err = r, err
	}
	return results
}

// ---------------------------------------------------------------------------
// PQ-accelerated search (single query)
// ---------------------------------------------------------------------------

func (m *cudaManager) pqSearch(d *gpuSegData, query []float32, topK, nProbe int, metric vse.MetricType) ([]vse.SearchResult, error) {
	if nProbe <= 0 {
		nProbe = m.cfg.NProbe
	}
	if topK <= 0 {
		topK = 10
	}
	if topK > d.nv {
		topK = d.nv
	}
	if nProbe > d.nc {
		nProbe = d.nc
	}

	if err := m.queryPool.ensureCapacity(d.dim, d.nv, d.M, d.nc); err != nil {
		return nil, err
	}

	dQ := m.queryPool.dQ
	dCDists := m.queryPool.dCDists
	dDists := m.queryPool.dDists
	dTable := m.queryPool.dTable

	// H2D: query（唯一的 H2D 传输）
	qBuf := make([]float32, d.dim)
	copy(qBuf, query)
	if err := cuMemcpyH2DAsync(dQ, unsafe.Pointer(&qBuf[0]), d.dim*4, m.stream); err != nil {
		return nil, err
	}

	// GPU: 质心距离
	m.launchCentroidDist(m.kernels.centroidDist, dQ, d.dCentroids, dCDists, 1, d.nc, d.dim)

	// GPU: 在 GPU 上直接选 top-nProbe 个质心（消除 D2H→CPU→H2D 往返）
	// centroid_select_topk kernel: 单 block, thread 0 扫描全部 nc
	var dProbeCentroids unsafe.Pointer
	if err := cuMalloc(&dProbeCentroids, nProbe*4); err != nil {
		return nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dProbeCentroids)))
	m.launchCentroidSelectTopk(m.kernels.centroidSelectTopK,
		dCDists, dProbeCentroids, d.nc, nProbe)

	// GPU: 从 IVF lists 收集候选索引
	var dCandIdx unsafe.Pointer
	if err := cuMalloc(&dCandIdx, d.nv*4); err != nil {
		return nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dCandIdx)))
	var dCandCount unsafe.Pointer
	if err := cuMalloc(&dCandCount, 4); err != nil {
		return nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dCandCount)))

	if err := m.launchGatherIVFCandidates(m.kernels.gatherIVFCandidates,
		dProbeCentroids, d.dClusterOffsets, d.dClusterMembers,
		dCandIdx, dCandCount, nProbe); err != nil {
		return nil, err
	}
	if err := cuStreamSync(m.stream); err != nil {
		return nil, err
	}

	// D2H: 候选数量（4 bytes）
	var nCand32 int32
	if err := cuMemcpyD2H(unsafe.Pointer(&nCand32), dCandCount, 4); err != nil {
		return nil, err
	}
	nCand := int(nCand32)
	if nCand == 0 {
		return nil, nil
	}

	rerankK := topK
	if m.cfg.RerankCandidates > 0 && d.dVectors != nil {
		rerankK = m.cfg.RerankCandidates
	}
	if rerankK > nCand {
		rerankK = nCand
	}

	// GPU: PQ 距离表
	m.launchPQDistTable(m.kernels.pqDistTable, dQ, d.dCodebook, dTable, 1, d.dim, d.M, d.subdim)

	// GPU: PQ-ADC gather（只计算 nCand 个候选的 PQ 距离）
	if err := m.launchPQADCGather(m.kernels.pqADCGather, dTable, d.dCodes, dCandIdx, dDists, 1, nCand, d.M); err != nil {
		return nil, err
	}

	// 生成 PQ 粗排候选列表（GPU topK 或 CPU fallback）
	// 两种路径都产生排序好的 allCands[0:rerankK]
	type cand struct {
		vecIdx int32
		dist   float32
	}
	const maxGpuTopKSmem = 4096
	var allCands []cand

	if nCand > rerankK && rerankK <= maxGpuTopKSmem {
		// GPU 路径：topk_select_indices → merge_topk_positions
		numBlocks := (nCand + 255) / 256

		var dBlockPos unsafe.Pointer
		if err := cuMalloc(&dBlockPos, numBlocks*rerankK*4); err != nil {
			return nil, err
		}
		defer C.cuMemFree(C.CUdeviceptr(uintptr(dBlockPos)))
		var dBlockCounts unsafe.Pointer
		if err := cuMalloc(&dBlockCounts, numBlocks*4); err != nil {
			return nil, err
		}
		defer C.cuMemFree(C.CUdeviceptr(uintptr(dBlockCounts)))

		m.launchTopKSelectIndices(m.kernels.topkSelectIndices,
			dDists, dBlockPos, dBlockCounts, nCand, rerankK)

		var dMergedDists, dMergedVecIdx unsafe.Pointer
		if err := cuMalloc(&dMergedDists, rerankK*4); err != nil {
			return nil, err
		}
		defer C.cuMemFree(C.CUdeviceptr(uintptr(dMergedDists)))
		if err := cuMalloc(&dMergedVecIdx, rerankK*4); err != nil {
			return nil, err
		}
		defer C.cuMemFree(C.CUdeviceptr(uintptr(dMergedVecIdx)))

		m.launchMergeTopKPositions(m.kernels.mergeTopKPositions,
			dBlockPos, dBlockCounts, dDists, dCandIdx,
			dMergedDists, dMergedVecIdx, numBlocks, rerankK)

		if err := cuStreamSync(m.stream); err != nil {
			return nil, err
		}

		mergedDists := make([]float32, rerankK)
		mergedVecIdx := make([]int32, rerankK)
		if err := cuMemcpyD2H(unsafe.Pointer(&mergedDists[0]), dMergedDists, rerankK*4); err != nil {
			return nil, err
		}
		if err := cuMemcpyD2H(unsafe.Pointer(&mergedVecIdx[0]), dMergedVecIdx, rerankK*4); err != nil {
			return nil, err
		}

		allCands = make([]cand, 0, rerankK)
		for i := 0; i < rerankK; i++ {
			if mergedVecIdx[i] < 0 {
				break
			}
			allCands = append(allCands, cand{vecIdx: mergedVecIdx[i], dist: mergedDists[i]})
		}
	} else {
		// CPU 路径：D2H 全部候选 + CPU 排序
		if err := cuStreamSync(m.stream); err != nil {
			return nil, err
		}

		pqDists := make([]float32, nCand)
		if err := cuMemcpyD2H(unsafe.Pointer(&pqDists[0]), dDists, nCand*4); err != nil {
			return nil, err
		}
		candIdxHost := make([]int32, nCand)
		if err := cuMemcpyD2H(unsafe.Pointer(&candIdxHost[0]), dCandIdx, nCand*4); err != nil {
			return nil, err
		}

		allCands = make([]cand, nCand)
		for i := 0; i < nCand; i++ {
			allCands[i] = cand{vecIdx: candIdxHost[i], dist: pqDists[i]}
		}
		sort.Slice(allCands, func(i, j int) bool { return allCands[i].dist < allCands[j].dist })
		if len(allCands) > rerankK {
			allCands = allCands[:rerankK]
		}
	}

	// 无 rerank：直接返回 PQ topK
	if m.cfg.RerankCandidates == 0 || d.dVectors == nil || len(allCands) <= topK {
		if len(allCands) > topK {
			allCands = allCands[:topK]
		}
		res := make([]vse.SearchResult, len(allCands))
		for i, c := range allCands {
			if int(c.vecIdx) >= 0 && int(c.vecIdx) < len(d.hostGids) {
				res[i] = vse.SearchResult{ID: vse.VectorID(d.hostGids[c.vecIdx]), Score: c.dist}
			}
		}
		return res, nil
	}

	// === Unified rerank 路径 ===
	rerankVecIdx := make([]int32, len(allCands))
	for i, c := range allCands {
		rerankVecIdx[i] = c.vecIdx
	}
	if err := m.queryPool.ensureRerankCapacity(len(rerankVecIdx)); err != nil {
		return nil, err
	}
	dRerankCandIdx := m.queryPool.dRerankIdx
	if err := cuMemcpyH2DAsync(dRerankCandIdx, unsafe.Pointer(&rerankVecIdx[0]), len(rerankVecIdx)*4, m.stream); err != nil {
		return nil, err
	}

	// GPU: gather 精确距离
	var qNorm2 float32
	for i := 0; i < d.dim; i++ {
		qNorm2 += query[i] * query[i]
	}
	dRerankDists := m.queryPool.dRerankD

	switch metric {
	case vse.MetricEuclidean:
		if err := m.launchGatherL2(m.kernels.gatherL2, d.dVectors, dRerankCandIdx, dQ, d.dNorms, dRerankDists, qNorm2, len(rerankVecIdx), d.dim); err != nil {
			return nil, err
		}
	case vse.MetricCosine:
		if err := m.launchGatherCosine(m.kernels.gatherCosine, d.dVectors, dRerankCandIdx, dQ, d.dNorms, dRerankDists, qNorm2, len(rerankVecIdx), d.dim); err != nil {
			return nil, err
		}
	default:
		if err := m.launchGatherDot(m.kernels.gatherDot, d.dVectors, dRerankCandIdx, dQ, dRerankDists, len(rerankVecIdx), d.dim); err != nil {
			return nil, err
		}
	}

	// GPU topk_select_gather 筛选 topK
	nCandRerank := len(rerankVecIdx)
	if nCandRerank > topK && topK <= maxGpuTopKSmem {
		numB := (nCandRerank + 255) / 256
		var dPartialDists, dPartialGids, dPartialCounts unsafe.Pointer
		if err := cuMalloc(&dPartialDists, numB*topK*4); err != nil {
			return nil, err
		}
		defer C.cuMemFree(C.CUdeviceptr(uintptr(dPartialDists)))
		if err := cuMalloc(&dPartialGids, numB*topK*8); err != nil {
			return nil, err
		}
		defer C.cuMemFree(C.CUdeviceptr(uintptr(dPartialGids)))
		if err := cuMalloc(&dPartialCounts, numB*4); err != nil {
			return nil, err
		}
		defer C.cuMemFree(C.CUdeviceptr(uintptr(dPartialCounts)))

		if err := m.launchTopKSelectGather(m.kernels.topkSelectGather,
			dRerankDists, dRerankCandIdx, d.dGlobalIDs,
			dPartialDists, dPartialGids, dPartialCounts,
			nCandRerank, topK); err != nil {
			return nil, err
		}
		if err := cuStreamSync(m.stream); err != nil {
			return nil, err
		}

		partialDists := make([]float32, numB*topK)
		partialGids := make([]uint64, numB*topK)
		partialCounts := make([]int32, numB)
		if err := cuMemcpyD2H(unsafe.Pointer(&partialDists[0]), dPartialDists, numB*topK*4); err != nil {
			return nil, err
		}
		if err := cuMemcpyD2H(unsafe.Pointer(&partialGids[0]), dPartialGids, numB*topK*8); err != nil {
			return nil, err
		}
		if err := cuMemcpyD2H(unsafe.Pointer(&partialCounts[0]), dPartialCounts, numB*4); err != nil {
			return nil, err
		}

		type svMerge struct {
			gid   uint64
			score float32
		}
		var merged []svMerge
		for b := 0; b < numB; b++ {
			cnt := int(partialCounts[b])
			for j := 0; j < cnt; j++ {
				merged = append(merged, svMerge{gid: partialGids[b*topK+j], score: partialDists[b*topK+j]})
			}
		}
		if metric == vse.MetricDotProduct {
			sort.Slice(merged, func(i, j int) bool { return merged[i].score > merged[j].score })
		} else {
			sort.Slice(merged, func(i, j int) bool { return merged[i].score < merged[j].score })
		}
		if len(merged) > topK {
			merged = merged[:topK]
		}

		res := make([]vse.SearchResult, len(merged))
		for i, s := range merged {
			res[i] = vse.SearchResult{ID: vse.VectorID(s.gid), Score: s.score}
		}
		return res, nil
	}

	// Fallback: D2H 全部 + CPU 排序
	if err := cuStreamSync(m.stream); err != nil {
		return nil, err
	}
	exactDists := make([]float32, nCandRerank)
	if err := cuMemcpyD2H(unsafe.Pointer(&exactDists[0]), dRerankDists, nCandRerank*4); err != nil {
		return nil, err
	}

	type svFallback struct {
		idx   int32
		score float32
	}
	all := make([]svFallback, nCandRerank)
	for i := 0; i < nCandRerank; i++ {
		all[i] = svFallback{idx: rerankVecIdx[i], score: exactDists[i]}
	}
	if metric == vse.MetricDotProduct {
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	} else {
		sort.Slice(all, func(i, j int) bool { return all[i].score < all[j].score })
	}
	if len(all) > topK {
		all = all[:topK]
	}

	res := make([]vse.SearchResult, len(all))
	for i, s := range all {
		if int(s.idx) >= 0 && int(s.idx) < len(d.hostGids) {
			res[i] = vse.SearchResult{ID: vse.VectorID(d.hostGids[s.idx]), Score: s.score}
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Batch PQ search
// ---------------------------------------------------------------------------

func (m *cudaManager) batchPQSearch(d *gpuSegData, reqs []SearchRequest) []BatchResult {
	nq := len(reqs)

	// Upload all queries as one flat buffer
	qFlat := make([]float32, nq*d.dim)
	for i, req := range reqs {
		copy(qFlat[i*d.dim:], req.Query)
	}
	var dQ unsafe.Pointer
	if err := cuMalloc(&dQ, nq*d.dim*4); err != nil {
		return errBatch(err)
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dQ)))
	if err := cuMemcpyH2D(dQ, unsafe.Pointer(&qFlat[0]), nq*d.dim*4); err != nil {
		return errBatch(err)
	}

	// Centroid distances via cuBLAS Sgemm
	var dCD unsafe.Pointer
	if err := cuMalloc(&dCD, nq*d.nc*4); err != nil {
		return errBatch(err)
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dCD)))

	alpha := C.float(-2.0)
	beta := C.float(0.0)
	C.cublasSgemm(m.handle,
		C.CUBLAS_OP_N, C.CUBLAS_OP_T,
		C.int(nq), C.int(d.nc), C.int(d.dim),
		&alpha,
		(*C.float)(dQ), C.int(nq),
		(*C.float)(d.dCentroids), C.int(d.nc),
		&beta,
		(*C.float)(dCD), C.int(nq),
	)

	// PQ distance tables
	var dTbl unsafe.Pointer
	if err := cuMalloc(&dTbl, nq*d.M*256*4); err != nil {
		return errBatch(err)
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dTbl)))

	m.launchPQDistTable(m.kernels.pqDistTable, dQ, d.dCodebook, dTbl, nq, d.dim, d.M, d.subdim)

	// PQ-ADC for all queries × all vectors
	var dDists unsafe.Pointer
	if err := cuMalloc(&dDists, nq*d.nv*4); err != nil {
		return errBatch(err)
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dDists)))

	threads := 128
	blocks := (d.nv + threads - 1) / threads
	if err := m.fastLaunch(m.kernels.pqADC,
		uint32(nq), uint32(blocks), uint32(threads), 1,
		ptrArg(dTbl), ptrArg(d.dCodes), ptrArg(dDists),
		intArg((*C.int)(unsafe.Pointer(&nq))),
		intArg((*C.int)(unsafe.Pointer(&d.nv))),
		intArg((*C.int)(unsafe.Pointer(&d.M))),
	); err != nil {
		return errBatch(err)
	}

	// GPU topK: 用 batched_topk_select_gids 直接在 GPU 上筛选 topK
	// 避免 D2H 全部 nq*nv 距离 + CPU 排序 + 冗余 D2H globalIDs
	topKMax := 0
	for _, req := range reqs {
		k := req.TopK
		if k <= 0 {
			k = 10
		}
		if k > topKMax {
			topKMax = k
		}
	}
	if topKMax > d.nv {
		topKMax = d.nv
	}

	var dOutDists, dOutGids unsafe.Pointer
	if err := cuMalloc(&dOutDists, nq*topKMax*4); err != nil {
		return errBatch(err)
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dOutDists)))
	if err := cuMalloc(&dOutGids, nq*topKMax*8); err != nil {
		return errBatch(err)
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dOutGids)))

	m.launchBatchTopKSelectGids(m.kernels.batchedTopKSelectGids,
		dDists, d.dGlobalIDs, dOutDists, dOutGids, nq, d.nv, topKMax)

	hostDists := make([]float32, nq*topKMax)
	hostGids := make([]uint64, nq*topKMax)
	if err := cuMemcpyD2H(unsafe.Pointer(&hostDists[0]), dOutDists, nq*topKMax*4); err != nil {
		return errBatch(err)
	}
	if err := cuMemcpyD2H(unsafe.Pointer(&hostGids[0]), dOutGids, nq*topKMax*8); err != nil {
		return errBatch(err)
	}

	results := make([]BatchResult, nq)
	for q := 0; q < nq; q++ {
		k := reqs[q].TopK
		if k <= 0 {
			k = 10
		}
		if k > topKMax {
			k = topKMax
		}

		rowDists := hostDists[q*topKMax : q*topKMax+k]
		rowGids := hostGids[q*topKMax : q*topKMax+k]

		res := make([]vse.SearchResult, k)
		for i := 0; i < k; i++ {
			res[i] = vse.SearchResult{ID: vse.VectorID(rowGids[i]), Score: rowDists[i]}
		}
		results[q].Results = res
	}
	return results
}

// ---------------------------------------------------------------------------
// Exact flat search (cuBLAS Sgemv)
// ---------------------------------------------------------------------------

func (m *cudaManager) flatSearch(d *gpuSegData, query []float32, topK int, metric vse.MetricType) ([]vse.SearchResult, error) {
	// 确保缓冲池容量足够
	if err := m.queryPool.ensureCapacity(d.dim, d.nv, 1, 1); err != nil {
		return nil, err
	}

	dQ := m.queryPool.dQ
	dDists := m.queryPool.dDists

	// 异步上传 query
	qBuf := make([]float32, d.dim)
	copy(qBuf, query)
	if err := cuMemcpyH2DAsync(dQ, unsafe.Pointer(&qBuf[0]), d.dim*4, m.stream); err != nil {
		return nil, err
	}

	// cuBLAS Sgemv: dDists = V^T * q (dot products)
	alpha := C.float(1.0)
	beta := C.float(0.0)
	C.cublasSgemv(m.handle, C.CUBLAS_OP_T,
		C.int(d.dim), C.int(d.nv), &alpha,
		(*C.float)(d.dVectors), C.int(d.dim),
		(*C.float)(dQ), C.int(1),
		&beta, (*C.float)(dDists), C.int(1))

	if metric == vse.MetricEuclidean {
		// GPU 端计算 L2^2 = qNorm2 + vNorm2 - 2*dot
		// 先计算 qNorm2
		var qNorm2 float32
		C.cublasSdot(m.handle, C.int(d.dim),
			(*C.float)(dQ), C.int(1), (*C.float)(dQ), C.int(1),
			(*C.float)(unsafe.Pointer(&qNorm2)))
		// 启动 L2 距离 kernel
		if err := m.launchL2FromDot(m.kernels.l2FromDot, dDists, d.dNorms, qNorm2, d.nv); err != nil {
			return nil, err
		}
		// GPU topK
		return m.gpuTopKResults(dDists, d, topK)
	}

	if metric == vse.MetricCosine {
		// GPU 端计算 cosine = 1 - dot / (||q|| * ||v||)
		var qNorm2 float32
		C.cublasSdot(m.handle, C.int(d.dim),
			(*C.float)(dQ), C.int(1), (*C.float)(dQ), C.int(1),
			(*C.float)(unsafe.Pointer(&qNorm2)))
		if err := m.launchCosineFromDot(m.kernels.cosineFromDot, dDists, d.dNorms, qNorm2, d.nv); err != nil {
			return nil, err
		}
		return m.gpuTopKResults(dDists, d, topK)
	}

	// Dot product: 直接 GPU topK
	return m.gpuTopKResults(dDists, d, topK)
}

// ---------------------------------------------------------------------------
// Vamana GPU support
// ---------------------------------------------------------------------------

// pinVamanaGraph uploads Vamana graph data (CSR neighbor offsets + data) to GPU.
func (m *cudaManager) pinVamanaGraph(segID vse.SegmentID, nbrOffsets, nbrData []int32) error {
	m.mu.Lock()
	unlock := m.cudaLock()
	defer unlock()
	defer m.mu.Unlock()

	d, ok := m.segments[uint32(segID)]
	if !ok {
		return fmt.Errorf("segment %d not found", segID)
	}
	if d.dNbrOffsets != nil {
		return nil // already pinned
	}

	if err := cuMalloc(&d.dNbrOffsets, len(nbrOffsets)*4); err != nil {
		return fmt.Errorf("alloc nbrOffsets: %w", err)
	}
	if err := cuMalloc(&d.dNbrData, len(nbrData)*4); err != nil {
		return fmt.Errorf("alloc nbrData: %w", err)
	}
	if err := cuMemcpyH2D(d.dNbrOffsets, unsafe.Pointer(&nbrOffsets[0]), len(nbrOffsets)*4); err != nil {
		return fmt.Errorf("copy nbrOffsets: %w", err)
	}
	if err := cuMemcpyH2D(d.dNbrData, unsafe.Pointer(&nbrData[0]), len(nbrData)*4); err != nil {
		return fmt.Errorf("copy nbrData: %w", err)
	}
	return nil
}

// hasVamanaGraph 检查 GPU 上是否存在 Vamana 图数据。
func (m *cudaManager) hasVamanaGraph(segID vse.SegmentID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.segments[uint32(segID)]
	if !ok {
		return false
	}
	return d.dNbrOffsets != nil && d.dNbrData != nil
}

// vamanaSearchGPU performs GPU-accelerated Vamana search for a single query.
// Iteratively expands frontier on GPU, returns candidate IDs for CPU rerank.
func (m *cudaManager) vamanaSearchGPU(segID vse.SegmentID, query []float32, topK, beamL int) (candIDs []int32, candDists []float32, _ error) {
	m.mu.Lock()
	threadUnlock := m.cudaLock()
	d, ok := m.segments[uint32(segID)]
	m.mu.Unlock()
	if !ok {
		threadUnlock()
		return nil, nil, fmt.Errorf("segment %d not on GPU", segID)
	}
	defer threadUnlock()

	if d.dNbrOffsets == nil || d.dNbrData == nil {
		return nil, nil, fmt.Errorf("vamana graph not pinned for segment %d", segID)
	}
	if beamL <= 0 {
		beamL = 64
	}
	if beamL > 64 {
		beamL = 64
	}

	// Upload query to GPU
	if err := m.queryPool.ensureCapacity(d.dim, d.nv, d.M, 1); err != nil {
		return nil, nil, err
	}
	qBuf := make([]float32, d.dim)
	copy(qBuf, query)
	if err := cuMemcpyH2DAsync(m.queryPool.dQ, unsafe.Pointer(&qBuf[0]), d.dim*4, m.stream); err != nil {
		return nil, nil, err
	}

	// Build PQ distance table on GPU
	if err := m.fastLaunch(m.kernels.pqDistTable,
		1, uint32(d.M), 256, 2,
		ptrArg(m.queryPool.dQ), ptrArg(d.dCodebook), ptrArg(m.queryPool.dTable),
		intArg((*C.int)(unsafe.Pointer(&dq1))),
		intArg((*C.int)(unsafe.Pointer(&d.dim))),
		intArg((*C.int)(unsafe.Pointer(&d.M))),
		intArg((*C.int)(unsafe.Pointer(&d.subdim))),
	); err != nil {
		return nil, nil, err
	}

	// Allocate frontier buffers
	const maxCand = 64 * 4096 // beamL * maxDegree per node
	dFrontierW := C.int(1)
	dFrontierIDs := make([]int32, beamL)
	dFrontierDists := make([]float32, beamL)

	var dFIDs, dFDists, dNewIDs, dNewDists, dCount unsafe.Pointer
	alloc := func(p *unsafe.Pointer, size int) error {
		if err := cuMalloc(p, size); err != nil {
			return fmt.Errorf("alloc %d: %w", size, err)
		}
		return nil
	}
	if err := alloc(&dFIDs, beamL*4); err != nil {
		return nil, nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dFIDs)))
	if err := alloc(&dFDists, beamL*4); err != nil {
		return nil, nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dFDists)))
	if err := alloc(&dNewIDs, maxCand*4); err != nil {
		return nil, nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dNewIDs)))
	if err := alloc(&dNewDists, maxCand*4); err != nil {
		return nil, nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dNewDists)))
	if err := alloc(&dCount, 4); err != nil {
		return nil, nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dCount)))

	dFrontierIDs[0] = 0 // start from node 0 (medoid)
	dFrontierDists[0] = 0
	if err := cuMemcpyH2D(dFIDs, unsafe.Pointer(&dFrontierIDs[0]), beamL*4); err != nil {
		return nil, nil, err
	}
	if err := cuMemcpyH2D(dFDists, unsafe.Pointer(&dFrontierDists[0]), beamL*4); err != nil {
		return nil, nil, err
	}

	// Iterative frontier expansion
	dNv := C.int(d.nv)
	dM := C.int(d.M)
	dBeamL := C.int(beamL)
	dMaxOut := C.int(maxCand)
	dFrontierW = C.int(1)

	maxIter := 6
	for iter := 0; iter < maxIter; iter++ {
		// Expand frontier on GPU: gather neighbors, compute PQ-ADC
		if err := m.fastLaunchSmem(m.kernels.vamanaExpand,
			1, 1, 256, 1, 0,
			ptrArg(dFIDs), ptrArg(d.dNbrOffsets), ptrArg(d.dNbrData),
			ptrArg(d.dCodes), ptrArg(m.queryPool.dTable),
			ptrArg(dNewIDs), ptrArg(dNewDists), ptrArg(dCount),
			intArg(&dFrontierW), intArg(&dNv), intArg(&dM), intArg(&dMaxOut),
		); err != nil {
			return nil, nil, err
		}

		// Read back count
		var newCount int32
		if err := cuMemcpyD2H(unsafe.Pointer(&newCount), dCount, 4); err != nil {
			return nil, nil, err
		}
		if newCount == 0 {
			break
		}

		// Merge old frontier with new candidates on GPU
		if err := m.fastLaunchSmem(m.kernels.vamanaMergeFrontier,
			1, 1, 256, 1, uint32(beamL*(4+4)),
			ptrArg(dFIDs), ptrArg(dFDists),
			ptrArg(dNewIDs), ptrArg(dNewDists),
			ptrArg(dFIDs), ptrArg(dFDists), ptrArg(dCount),
			intArg(&dFrontierW), intArg(&dFrontierW), intArg(&dBeamL),
		); err != nil {
			return nil, nil, err
		}

		// Read back merged count for next iteration
		var mergedCount int32
		if err := cuMemcpyD2H(unsafe.Pointer(&mergedCount), dCount, 4); err != nil {
			return nil, nil, err
		}
		dFrontierW = C.int(mergedCount)
		if dFrontierW == 0 {
			break
		}
	}

	// Final frontier readback
	finalCount := int(dFrontierW)
	if finalCount > topK*2 {
		finalCount = topK * 2
	}
	finalIDs := make([]int32, finalCount)
	finalDists := make([]float32, finalCount)
	if err := cuMemcpyD2H(unsafe.Pointer(&finalIDs[0]), dFIDs, finalCount*4); err != nil {
		return nil, nil, err
	}
	if err := cuMemcpyD2H(unsafe.Pointer(&finalDists[0]), dFDists, finalCount*4); err != nil {
		return nil, nil, err
	}
	return finalIDs, finalDists, nil
}

var dq1 C.int = 1 // single query constant

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func selectCandidates(cdists []float32, nProbe int, d *gpuSegData) []int {
	if len(d.clusterOffsets) == 0 || len(d.clusterMembers) == 0 {
		return nil
	}
	nc := len(d.clusterOffsets) - 1
	if nProbe > nc {
		nProbe = nc
	}

	// Sort centroids by distance, pick top nProbe
	type centroid struct {
		idx  int
		dist float32
	}
	centroids := make([]centroid, nc)
	for i := 0; i < nc; i++ {
		centroids[i] = centroid{i, cdists[i]}
	}
	sort.Slice(centroids, func(i, j int) bool {
		return centroids[i].dist < centroids[j].dist
	})
	if nProbe < nc {
		centroids = centroids[:nProbe]
	}

	// Count total candidates and collect member indices
	total := 0
	for _, c := range centroids {
		total += int(d.clusterOffsets[c.idx+1] - d.clusterOffsets[c.idx])
	}
	if total == 0 {
		return nil
	}

	candidates := make([]int, 0, total)
	for _, c := range centroids {
		start := d.clusterOffsets[c.idx]
		end := d.clusterOffsets[c.idx+1]
		for _, v := range d.clusterMembers[start:end] {
			candidates = append(candidates, int(v))
		}
	}
	return candidates
}

func makeRange(n int) []int {
	r := make([]int, n)
	for i := range r {
		r[i] = i
	}
	return r
}

func topKResults(dists []float32, indices []int, d *gpuSegData, topK int) ([]vse.SearchResult, error) {
	n := len(dists)
	type sv struct {
		idx   int
		score float32
	}
	all := make([]sv, n)
	for i, d := range dists {
		var realIdx int
		if indices != nil {
			realIdx = indices[i]
		} else {
			realIdx = i
		}
		all[i] = sv{realIdx, d}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score < all[j].score })
	if len(all) > topK {
		all = all[:topK]
	}

	// Read global IDs
	gids := make([]uint64, d.nv)
	if err := cuMemcpyD2H(unsafe.Pointer(&gids[0]), d.dGlobalIDs, d.nv*8); err != nil {
		return nil, err
	}

	res := make([]vse.SearchResult, len(all))
	for i, s := range all {
		gid := uint64(0)
		if s.idx < len(gids) {
			gid = gids[s.idx]
		}
		res[i] = vse.SearchResult{ID: vse.VectorID(gid), Score: s.score}
	}
	return res, nil
}

// gpuTopKCandidates 在 GPU 上执行 topK 筛选，返回候选的 local index + 距离
// 用于 rerank 流程：先取 Top R 个候选，再用精确向量 rerank
// 当 topK > 4096 时回退到 D2H + CPU 排序（shared memory 限制 48KB）
func (m *cudaManager) gpuTopKCandidates(dDists unsafe.Pointer, d *gpuSegData, topK int) ([]int, []float32, error) {
	if topK > d.nv {
		topK = d.nv
	}

	// shared memory 限制：topK * (4+8) <= 48KB → topK <= 4096
	const maxGpuTopK = 4096

	if topK > maxGpuTopK {
		return m.cpuTopKCandidates(dDists, d, topK)
	}

	threads := 256
	numBlocks := (d.nv + threads - 1) / threads

	var dOutDists unsafe.Pointer
	if err := cuMalloc(&dOutDists, numBlocks*topK*4); err != nil {
		return nil, nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dOutDists)))

	var dOutGids unsafe.Pointer
	if err := cuMalloc(&dOutGids, numBlocks*topK*8); err != nil {
		return nil, nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dOutGids)))

	var dOutCounts unsafe.Pointer
	if err := cuMalloc(&dOutCounts, numBlocks*4); err != nil {
		return nil, nil, err
	}
	defer C.cuMemFree(C.CUdeviceptr(uintptr(dOutCounts)))

	if err := m.launchTopKSelect(m.kernels.topkSelect, dDists, d.dGlobalIDs, dOutDists, dOutGids, dOutCounts, d.nv, topK); err != nil {
		return nil, nil, err
	}
	if err := cuStreamSync(m.stream); err != nil {
		return nil, nil, err
	}

	outDists := make([]float32, numBlocks*topK)
	if err := cuMemcpyD2H(unsafe.Pointer(&outDists[0]), dOutDists, numBlocks*topK*4); err != nil {
		return nil, nil, err
	}
	outGids := make([]uint64, numBlocks*topK)
	if err := cuMemcpyD2H(unsafe.Pointer(&outGids[0]), dOutGids, numBlocks*topK*8); err != nil {
		return nil, nil, err
	}
	outCounts := make([]int32, numBlocks)
	if err := cuMemcpyD2H(unsafe.Pointer(&outCounts[0]), dOutCounts, numBlocks*4); err != nil {
		return nil, nil, err
	}

	type sv struct {
		gid   uint64
		score float32
	}
	var merged []sv
	for b := 0; b < numBlocks; b++ {
		cnt := int(outCounts[b])
		for j := 0; j < cnt; j++ {
			idx := b*topK + j
			merged = append(merged, sv{gid: outGids[idx], score: outDists[idx]})
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].score < merged[j].score })
	if len(merged) > topK {
		merged = merged[:topK]
	}

	// 用缓存的 hostGids 构建 gid→local index 映射
	gidToLocal := make(map[uint64]int, len(d.hostGids))
	for i, gid := range d.hostGids {
		gidToLocal[gid] = i
	}

	indices := make([]int, len(merged))
	dists := make([]float32, len(merged))
	for i, s := range merged {
		indices[i] = gidToLocal[s.gid]
		dists[i] = s.score
	}
	return indices, dists, nil
}

// cpuTopKCandidates D2H 传输全部距离 + CPU 排序取 topK
// 用于 topK > 4096 的情况（GPU shared memory 不足）
func (m *cudaManager) cpuTopKCandidates(dDists unsafe.Pointer, d *gpuSegData, topK int) ([]int, []float32, error) {
	allDists := make([]float32, d.nv)
	if err := cuMemcpyD2H(unsafe.Pointer(&allDists[0]), dDists, d.nv*4); err != nil {
		return nil, nil, err
	}

	type sv struct {
		idx   int
		score float32
	}
	all := make([]sv, d.nv)
	for i, dist := range allDists {
		all[i] = sv{idx: i, score: dist}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score < all[j].score })
	if len(all) > topK {
		all = all[:topK]
	}

	indices := make([]int, len(all))
	dists := make([]float32, len(all))
	for i, s := range all {
		indices[i] = s.idx
		dists[i] = s.score
	}
	return indices, dists, nil
}

// gpuTopKResults 在 GPU 上执行 topK 筛选，返回最终结果
// 用于 flatSearch（精确搜索，无需 rerank）
func (m *cudaManager) gpuTopKResults(dDists unsafe.Pointer, d *gpuSegData, topK int) ([]vse.SearchResult, error) {
	candIndices, dists, err := m.gpuTopKCandidates(dDists, d, topK)
	if err != nil {
		return nil, err
	}
	res := make([]vse.SearchResult, len(candIndices))
	for i, idx := range candIndices {
		res[i] = vse.SearchResult{ID: vse.VectorID(d.hostGids[idx]), Score: dists[i]}
	}
	return res, nil
}

// rerankExact 全 GPU 距离计算 + CPU topK 排序，缓冲区复用
func (m *cudaManager) rerankExact(d *gpuSegData, query []float32, candIndices []int, topK int, metric vse.MetricType) ([]vse.SearchResult, error) {
	nCand := len(candIndices)
	if nCand == 0 {
		return nil, nil
	}
	if topK > nCand {
		topK = nCand
	}

	// 复用内存池缓冲区（避免每次 cuMalloc/cuMemFree）
	if err := m.queryPool.ensureRerankCapacity(nCand); err != nil {
		return nil, err
	}
	dCandIdx := m.queryPool.dRerankIdx
	dDists := m.queryPool.dRerankD

	// 上传候选索引
	candI32 := make([]int32, nCand)
	for i, v := range candIndices {
		candI32[i] = int32(v)
	}
	if err := cuMemcpyH2DAsync(dCandIdx, unsafe.Pointer(&candI32[0]), nCand*4, m.stream); err != nil {
		return nil, err
	}

	// 计算 qNorm2
	var qNorm2 float32
	for i := 0; i < d.dim; i++ {
		qNorm2 += query[i] * query[i]
	}

	dQ := m.queryPool.dQ

	// Step 1: gather + 距离计算（GPU kernel）
	if metric == vse.MetricDotProduct {
		if err := m.launchGatherDot(m.kernels.gatherDot, d.dVectors, dCandIdx, dQ, dDists, nCand, d.dim); err != nil {
			return nil, err
		}
	} else if metric == vse.MetricEuclidean {
		if err := m.launchGatherL2(m.kernels.gatherL2, d.dVectors, dCandIdx, dQ, d.dNorms, dDists, qNorm2, nCand, d.dim); err != nil {
			return nil, err
		}
	} else {
		if err := m.launchGatherCosine(m.kernels.gatherCosine, d.dVectors, dCandIdx, dQ, d.dNorms, dDists, qNorm2, nCand, d.dim); err != nil {
			return nil, err
		}
	}
	if err := cuStreamSync(m.stream); err != nil {
		return nil, err
	}

	// Step 2: D2H 距离（nCand * 4 bytes，10000 个 = 40KB）
	dists := make([]float32, nCand)
	if err := cuMemcpyD2H(unsafe.Pointer(&dists[0]), dDists, nCand*4); err != nil {
		return nil, err
	}

	// Step 3: CPU 排序取 topK
	type sv struct {
		idx   int
		score float32
	}
	all := make([]sv, nCand)
	for i := 0; i < nCand; i++ {
		all[i] = sv{idx: candIndices[i], score: dists[i]}
	}
	if metric == vse.MetricDotProduct {
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	} else {
		sort.Slice(all, func(i, j int) bool { return all[i].score < all[j].score })
	}
	if len(all) > topK {
		all = all[:topK]
	}

	res := make([]vse.SearchResult, len(all))
	for i, s := range all {
		res[i] = vse.SearchResult{ID: vse.VectorID(d.hostGids[s.idx]), Score: s.score}
	}
	return res, nil
}

func topKFromDistRow(dists []float32, d *gpuSegData, topK int) []vse.SearchResult {
	// Same as topKResults but without error return
	n := len(dists)
	type sv struct {
		idx   int
		score float32
	}
	all := make([]sv, n)
	for i, d := range dists {
		all[i] = sv{i, d}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score < all[j].score })
	if len(all) > topK {
		all = all[:topK]
	}

	// Use cached hostGids instead of repeated D2H
	res := make([]vse.SearchResult, len(all))
	for i, s := range all {
		if s.idx < len(d.hostGids) {
			res[i] = vse.SearchResult{ID: vse.VectorID(d.hostGids[s.idx]), Score: s.score}
		}
	}
	return res
}

func errBatch(err error) []BatchResult { return []BatchResult{{Err: err}} }

// ---------------------------------------------------------------------------
// Launch wrappers for each kernel
// ---------------------------------------------------------------------------

func (m *cudaManager) launchCentroidDist(fn C.CUfunction, dQ, dCent, dDists unsafe.Pointer, nq, nc, dim int) {
	cnq := C.int(nq)
	cnc := C.int(nc)
	cdim := C.int(dim)
	m.fastLaunch(fn, uint32(nq), uint32(nc), 256, 1,
		ptrArg(dQ), ptrArg(dCent), ptrArg(dDists),
		intArg(&cnq), intArg(&cnc), intArg(&cdim),
	)
}

func (m *cudaManager) launchPQDistTable(fn C.CUfunction, dQ, dCB, dTbl unsafe.Pointer, nq, dim, M, subdim int) {
	cnq := C.int(nq)
	cdim := C.int(dim)
	cM := C.int(M)
	csub := C.int(subdim)
	m.fastLaunch(fn, uint32(nq), uint32(M), 256, 2,
		ptrArg(dQ), ptrArg(dCB), ptrArg(dTbl),
		intArg(&cnq), intArg(&cdim), intArg(&cM), intArg(&csub),
	)
}

func (m *cudaManager) launchPQADC(fn C.CUfunction, dTbl, dCodes, dDists unsafe.Pointer, nq, nv, M int) error {
	threads := 128
	blocks := (nv + threads - 1) / threads
	cnq := C.int(nq)
	cnv := C.int(nv)
	cM := C.int(M)
	return m.fastLaunch(fn, uint32(nq), uint32(blocks), uint32(threads), 1,
		ptrArg(dTbl), ptrArg(dCodes), ptrArg(dDists),
		intArg(&cnq), intArg(&cnv), intArg(&cM),
	)
}

func (m *cudaManager) launchPQADCGather(fn C.CUfunction, dTbl, dCodes, dCands, dDists unsafe.Pointer, nq, nCand, M int) error {
	threads := 128
	blocks := (nCand + threads - 1) / threads
	cnq := C.int(nq)
	cnCand := C.int(nCand)
	cM := C.int(M)
	return m.fastLaunch(fn, uint32(nq), uint32(blocks), uint32(threads), 1,
		ptrArg(dTbl), ptrArg(dCodes), ptrArg(dCands), ptrArg(dDists),
		intArg(&cnq), intArg(&cnCand), intArg(&cM),
	)
}

func (m *cudaManager) launchNorms(fn C.CUfunction, dVecs, dNorms unsafe.Pointer, nv, dim int) {
	threads := 256
	blocks := (nv + threads - 1) / threads
	cnv := C.int(nv)
	cdim := C.int(dim)
	m.fastLaunch(fn, uint32(blocks), 1, uint32(threads), 1,
		ptrArg(dVecs), ptrArg(dNorms), intArg(&cnv), intArg(&cdim),
	)
}

// launchL2FromDot 将 dot product 转换为 L2^2 距离
func (m *cudaManager) launchL2FromDot(fn C.CUfunction, dDists, dNorms2 unsafe.Pointer, qNorm2 float32, nv int) error {
	threads := 256
	blocks := (nv + threads - 1) / threads
	cnv := C.int(nv)
	cqNorm := C.float(qNorm2)
	return m.fastLaunch(fn, uint32(blocks), 1, uint32(threads), 1,
		ptrArg(dDists), ptrArg(dNorms2), floatArg(&cqNorm), intArg(&cnv),
	)
}

// launchCosineFromDot 将 dot product 转换为 cosine 距离
func (m *cudaManager) launchCosineFromDot(fn C.CUfunction, dDists, dNorms2 unsafe.Pointer, qNorm2 float32, nv int) error {
	threads := 256
	blocks := (nv + threads - 1) / threads
	cnv := C.int(nv)
	cqNorm := C.float(qNorm2)
	return m.fastLaunch(fn, uint32(blocks), 1, uint32(threads), 1,
		ptrArg(dDists), ptrArg(dNorms2), floatArg(&cqNorm), intArg(&cnv),
	)
}

// launchGatherDot 对候选向量 gather + dot product
// 每个 thread 处理一个候选，直接从 dVectors 按索引读取，无需 D2D 拷贝
func (m *cudaManager) launchGatherDot(fn C.CUfunction, dVecs, dCandIdx, dQuery, dDots unsafe.Pointer, nCand, dim int) error {
	threads := 256
	blocks := (nCand + threads - 1) / threads
	cn := C.int(nCand)
	cd := C.int(dim)
	return m.fastLaunch(fn, uint32(blocks), 1, uint32(threads), 1,
		ptrArg(dVecs), ptrArg(dCandIdx), ptrArg(dQuery), ptrArg(dDots),
		intArg(&cn), intArg(&cd),
	)
}

// launchGatherL2 对候选向量 gather + L2^2 距离计算
func (m *cudaManager) launchGatherL2(fn C.CUfunction, dVecs, dCandIdx, dQuery, dNorms2, dDists unsafe.Pointer, qNorm2 float32, nCand, dim int) error {
	threads := 256
	blocks := (nCand + threads - 1) / threads
	cn := C.int(nCand)
	cd := C.int(dim)
	cqNorm := C.float(qNorm2)
	return m.fastLaunch(fn, uint32(blocks), 1, uint32(threads), 1,
		ptrArg(dVecs), ptrArg(dCandIdx), ptrArg(dQuery), ptrArg(dNorms2), ptrArg(dDists),
		floatArg(&cqNorm), intArg(&cn), intArg(&cd),
	)
}

// launchGatherCosine 对候选向量 gather + cosine 距离计算
func (m *cudaManager) launchGatherCosine(fn C.CUfunction, dVecs, dCandIdx, dQuery, dNorms2, dDists unsafe.Pointer, qNorm2 float32, nCand, dim int) error {
	threads := 256
	blocks := (nCand + threads - 1) / threads
	cn := C.int(nCand)
	cd := C.int(dim)
	cqNorm := C.float(qNorm2)
	return m.fastLaunch(fn, uint32(blocks), 1, uint32(threads), 1,
		ptrArg(dVecs), ptrArg(dCandIdx), ptrArg(dQuery), ptrArg(dNorms2), ptrArg(dDists),
		floatArg(&cqNorm), intArg(&cn), intArg(&cd),
	)
}

// launchTopKSelectGather 对候选子集做 topK 筛选，用 candIndices 映射回 global ID
func (m *cudaManager) launchTopKSelectGather(fn C.CUfunction, dDists, dCandIdx, dGids, dOutDists, dOutGids, dOutCounts unsafe.Pointer, nCand, topK int) error {
	threads := 256
	blocks := (nCand + threads - 1) / threads
	cn := C.int(nCand)
	ck := C.int(topK)
	smemBytes := uint32(topK) * (4 + 8)
	return m.fastLaunchSmem(fn, uint32(blocks), 1, uint32(threads), 1, smemBytes,
		ptrArg(dDists), ptrArg(dCandIdx), ptrArg(dGids), ptrArg(dOutDists), ptrArg(dOutGids), ptrArg(dOutCounts),
		intArg(&cn), intArg(&ck),
	)
}

// launchTopKSelectIndices 对距离数组做 topK，输出候选索引（而非 global ID）
func (m *cudaManager) launchTopKSelectIndices(fn C.CUfunction, dDists, dOutIndices, dOutCounts unsafe.Pointer, n, topK int) error {
	threads := 256
	blocks := (n + threads - 1) / threads
	cn := C.int(n)
	ck := C.int(topK)
	smemBytes := uint32(topK) * (4 + 4) // float + int
	return m.fastLaunchSmem(fn, uint32(blocks), 1, uint32(threads), 1, smemBytes,
		ptrArg(dDists), ptrArg(dOutIndices), ptrArg(dOutCounts),
		intArg(&cn), intArg(&ck),
	)
}

// launchGatherIVFCandidates 从 IVF lists 收集候选索引
func (m *cudaManager) launchGatherIVFCandidates(fn C.CUfunction, dProbeCentroids, dClusterOffsets, dClusterMembers, dOutCandidates, dOutCount unsafe.Pointer, nProbe int) error {
	threads := 256
	blocks := 1 // nProbe <= 256, single block
	cn := C.int(nProbe)
	return m.fastLaunch(fn, uint32(blocks), 1, uint32(threads), 1,
		ptrArg(dProbeCentroids), ptrArg(dClusterOffsets), ptrArg(dClusterMembers),
		ptrArg(dOutCandidates), ptrArg(dOutCount), intArg(&cn),
	)
}

// launchTopKSelect 在 GPU 上执行 topK 筛选
// 返回 numBlocks * topK 个候选，需在 CPU 上合并
func (m *cudaManager) launchTopKSelect(fn C.CUfunction, dDists, dGids, dOutDists, dOutGids unsafe.Pointer, dOutCounts unsafe.Pointer, nv, topK int) error {
	threads := 256
	blocks := (nv + threads - 1) / threads
	cnv := C.int(nv)
	ck := C.int(topK)
	// shared memory: topK * (sizeof(float) + sizeof(uint64))
	smemBytes := uint32(topK) * (4 + 8)
	return m.fastLaunchSmem(fn, uint32(blocks), 1, uint32(threads), 1, smemBytes,
		ptrArg(dDists), ptrArg(dGids), ptrArg(dOutDists), ptrArg(dOutGids), ptrArg(dOutCounts),
		intArg(&cnv), intArg(&ck),
	)
}

// launchBatchTopKSelectGids 批量 topK 选择：每查询一行一个 block
// grid = (nq, 1, 1), block = (256, 1, 1)
// shared memory: topK * (float + uint64) bytes
// 输出直接包含 global ID + 距离，已排序升序
func (m *cudaManager) launchBatchTopKSelectGids(fn C.CUfunction, dDists, dGids, dOutDists, dOutGids unsafe.Pointer, nq, nv, topK int) {
	cnq := C.int(nq)
	cnv := C.int(nv)
	ck := C.int(topK)
	smemBytes := uint32(topK) * (4 + 8)
	m.fastLaunchSmem(fn, uint32(nq), 1, 256, 1, smemBytes,
		ptrArg(dDists), ptrArg(dGids), ptrArg(dOutDists), ptrArg(dOutGids),
		intArg(&cnq), intArg(&cnv), intArg(&ck),
	)
}

// launchMergeTopKPositions 合并 topk_select_indices 的局部结果，输出最终 topK
func (m *cudaManager) launchMergeTopKPositions(fn C.CUfunction, dBlockPos, dBlockCounts, dOrigDists, dCandIdx, dOutDists, dOutVecIdx unsafe.Pointer, numBlocks, topK int) {
	cnb := C.int(numBlocks)
	ck := C.int(topK)
	smemBytes := uint32(topK) * (4 + 4)
	m.fastLaunchSmem(fn, 1, 1, 256, 1, smemBytes,
		ptrArg(dBlockPos), ptrArg(dBlockCounts), ptrArg(dOrigDists), ptrArg(dCandIdx),
		ptrArg(dOutDists), ptrArg(dOutVecIdx),
		intArg(&cnb), intArg(&ck),
	)
}

// launchCentroidSelectTopk 从质心距离数组中选择 topK 个最近质心索引
// grid = (1, 1, 1), block = (256, 1, 1)
// 直接输出质心索引，无需 CPU 中转
func (m *cudaManager) launchCentroidSelectTopk(fn C.CUfunction, dCDists, dOutIndices unsafe.Pointer, nc, topK int) {
	cnc := C.int(nc)
	ck := C.int(topK)
	smemBytes := uint32(topK) * (4 + 4)
	m.fastLaunchSmem(fn, 1, 1, 256, 1, smemBytes,
		ptrArg(dCDists), ptrArg(dOutIndices),
		intArg(&cnc), intArg(&ck),
	)
}

// ---------------------------------------------------------------------------
// CUDA error helpers
// ---------------------------------------------------------------------------

func cuCheck(err C.CUresult) error {
	if err == C.CUDA_SUCCESS {
		return nil
	}
	return fmt.Errorf("CUDA error %d", int(err))
}

func cuBlasCheck(err C.cublasStatus_t) error {
	if err == C.CUBLAS_STATUS_SUCCESS {
		return nil
	}
	return fmt.Errorf("cuBLAS error %d", int(err))
}

func cuMalloc(ptr *unsafe.Pointer, size int) error {
	if size <= 0 {
		return nil
	}
	var dp C.CUdeviceptr
	if err := cuCheck(C.cuMemAlloc(&dp, C.size_t(size))); err != nil {
		return err
	}
	*ptr = unsafe.Pointer(uintptr(dp))
	return nil
}

func cuMemcpyH2D(dst unsafe.Pointer, src unsafe.Pointer, size int) error {
	return cuCheck(C.cuMemcpyHtoD(C.CUdeviceptr(uintptr(dst)), src, C.size_t(size)))
}

func cuMemcpyD2H(dst unsafe.Pointer, src unsafe.Pointer, size int) error {
	return cuCheck(C.cuMemcpyDtoH(dst, C.CUdeviceptr(uintptr(src)), C.size_t(size)))
}

// cuMemcpyH2DAsync 异步主机→设备拷贝
func cuMemcpyH2DAsync(dst unsafe.Pointer, src unsafe.Pointer, size int, stream C.CUstream) error {
	return cuCheck(C.memcpyH2DAsync(C.CUdeviceptr(uintptr(dst)), src, C.size_t(size), stream))
}

// cuMemcpyD2HAsync 异步设备→主机拷贝
func cuMemcpyD2HAsync(dst unsafe.Pointer, src unsafe.Pointer, size int, stream C.CUstream) error {
	return cuCheck(C.memcpyD2HAsync(dst, C.CUdeviceptr(uintptr(src)), C.size_t(size), stream))
}

// cuStreamSync 等待流完成
func cuStreamSync(stream C.CUstream) error {
	return cuCheck(C.cuStreamSynchronize(stream))
}
