package vector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"cipherlake/internal/vector/vse"
	"cipherlake/internal/vector/vse/engine/cache"
	"cipherlake/internal/vector/vse/engine/segment"
	"cipherlake/internal/vector/vse/gpu"
	"cipherlake/internal/vector/vse/planner"
	"cipherlake/internal/vector/vse/prefetch"
)

type VSEBackend struct {
	mu           sync.RWMutex
	config       *vse.VSEConfig
	dim          int
	metric       MetricType
	metricFunc   func(a, b []float32) float32
	segManager   *segment.SegmentManager
	boltStore    *vse.BoltStore
	idGen        *vse.IDGenerator
	blockCache   *cache.ARC
	nextSegID    vse.SegmentID
	hotVecCount  atomic.Int64
	coldVecCount atomic.Int64
	queryCount   atomic.Int64
	totalLatNs   atomic.Int64
	s3FetchCount atomic.Int64
	stopped      chan struct{}

	// 延迟采样环形缓冲区，用于计算 P50/P99
	latencySamples []int64
	latencyIdx     int64
	latencyMu      sync.Mutex

	prefetch *prefetch.Engine
	planner  *planner.Planner
}

// latencySampleSize 控制延迟采样窗口大小
const latencySampleSize = 1024

func NewVSEBackend(dim int, metric MetricType, cfg *vse.VSEConfig, boltStore *vse.BoltStore) (*VSEBackend, error) {
	vsecfg := &vse.VSEConfig{
		DataDir:         cfg.DataDir,
		QuantizerType:   cfg.QuantizerType,
		CacheSize:       cfg.CacheSize,
		HNSWM:           cfg.HNSWM,
		HNSWEfSearch:    cfg.HNSWEfSearch,
		SearchWorkers:   cfg.SearchWorkers,
		GPUAccel:        cfg.GPUAccel,
		GPUBatchMin:     cfg.GPUBatchMin,
		IVFCentroids:    cfg.IVFCentroids,
		IVFNProbe:       cfg.IVFNProbe,
		PQSubQuantizers: cfg.PQSubQuantizers,
		PQBits:          cfg.PQBits,
		EnableS3:        cfg.EnableS3,
		S3Endpoint:      cfg.S3Endpoint,
		S3Region:        cfg.S3Region,
		S3Bucket:        cfg.S3Bucket,
		S3AccessKey:     cfg.S3AccessKey,
		S3SecretKey:     cfg.S3SecretKey,
		MergePolicy:     cfg.MergePolicy,
		WarmupPolicy:    cfg.WarmupPolicy,
	}

	if vsecfg.DataDir == "" {
		vsecfg.DataDir = "data/vector"
	}
	if vsecfg.MergePolicy.MergeInterval <= 0 {
		vsecfg.MergePolicy.MergeInterval = 5 * time.Minute
	}
	defaultMergePolicy := vse.DefaultMergePolicy()
	if vsecfg.MergePolicy.HotTargetSize <= 0 {
		vsecfg.MergePolicy.HotTargetSize = defaultMergePolicy.HotTargetSize
	}
	if vsecfg.MergePolicy.HotMaxSegments <= 0 {
		vsecfg.MergePolicy.HotMaxSegments = defaultMergePolicy.HotMaxSegments
	}
	if vsecfg.MergePolicy.ColdTargetSize <= 0 {
		vsecfg.MergePolicy.ColdTargetSize = defaultMergePolicy.ColdTargetSize
	}
	if vsecfg.MergePolicy.ColdMaxSegments <= 0 {
		vsecfg.MergePolicy.ColdMaxSegments = defaultMergePolicy.ColdMaxSegments
	}

	var mf vse.MetricType
	switch metric {
	case MetricEuclidean:
		mf = vse.MetricEuclidean
	case MetricDotProduct:
		mf = vse.MetricDotProduct
	default:
		mf = vse.MetricCosine
	}

	distFunc := vse.NewMetricFunc(mf)

	if err := os.MkdirAll(vsecfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	segDir := filepath.Join(vsecfg.DataDir, "segments")
	if err := os.MkdirAll(segDir, 0755); err != nil {
		return nil, fmt.Errorf("create segments dir: %w", err)
	}
	cacheDir := filepath.Join(vsecfg.DataDir, "prefetch_cache")
	os.MkdirAll(cacheDir, 0755)

	var gpuMgr *gpu.Manager
	if cfg.GPUAccel {
		gpuCfg := gpu.DefaultConfig()
		gpuCfg.Enabled = true
		if cfg.GPUBatchMin > 0 {
			gpuCfg.BatchThreshold = cfg.GPUBatchMin
		}
		gpuMgr = gpu.NewManager(gpuCfg)
		if err := gpuMgr.Init(); err != nil {
			fmt.Printf("[vse] gpu accel init: %v (falling back to CPU)\n", err)
			gpuMgr = nil
		} else {
			fmt.Printf("[vse] gpu acceleration enabled\n")
		}
	}

	segManager := segment.NewSegmentManager(segDir, distFunc, gpuMgr)

	prefetchEngine := prefetch.NewEngine(4, cacheDir, 512<<20)

	queryPlanner := planner.NewPlanner(cfg.SearchWorkers)
	queryPlanner.SetSegmentManager(segManager)

	vb := &VSEBackend{
		config:     vsecfg,
		dim:        dim,
		metric:     metric,
		metricFunc: distFunc,
		segManager: segManager,
		boltStore:  boltStore,
		idGen:      &vse.IDGenerator{},
		blockCache: cache.NewARC(vsecfg.CacheSize),
		nextSegID:  1,
		stopped:    make(chan struct{}),
		prefetch:   prefetchEngine,
		planner:    queryPlanner,
	}

	idGenVal := boltStore.GetIDGen()
	vb.idGen.AdvanceTo(idGenVal)

	metas, err := boltStore.ListSegments()
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}

	for _, meta := range metas {
		_, err := segManager.LoadSegment(context.Background(), meta)
		if err != nil {
			continue
		}
		if meta.ID >= vb.nextSegID {
			vb.nextSegID = meta.ID + 1
		}
		if meta.Tier == vse.TierHot {
			vb.hotVecCount.Add(int64(meta.NumVectors))
		} else {
			vb.coldVecCount.Add(int64(meta.NumVectors))
		}
	}

	queryPlanner.UpdateSegments(segManager.ListHotSegments(), segManager.ListColdSegments())

	go vb.warmup(context.Background())
	go vb.mergeLoop()

	return vb, nil
}

func NewBoltStore(path string) (*vse.BoltStore, error) {
	return vse.NewBoltStore(path)
}

func (vb *VSEBackend) Insert(_ context.Context, vectors []Vector) error {
	vb.mu.Lock()
	defer vb.mu.Unlock()

	if len(vectors) == 0 {
		return nil
	}

	entries := make([]vse.VectorEntry, len(vectors))
	for i, v := range vectors {
		id := vb.idGen.Next()
		var meta map[string]string
		if v.Metadata != nil {
			meta = v.Metadata
		}
		entries[i] = vse.VectorEntry{
			ID:        id,
			ExtID:     v.ID,
			Values:    v.Values,
			Metadata:  meta,
			Bucket:    v.Bucket,
			ObjectKey: v.ObjectKey,
			CreatedAt: time.Now(),
		}
	}

	segID := vb.nextSegID
	dir := filepath.Join(vb.config.DataDir, "segments", fmt.Sprintf("seg_%04d", segID))

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create segment dir: %w", err)
	}

	vmeta := &vse.SegmentMeta{
		ID:         segID,
		Tier:       vse.TierHot,
		NumVectors: len(entries),
		Dimension:  vb.dim,
		CreatedAt:  time.Now(),
		DataSize:   int64(len(entries) * (8 + 8 + vb.dim*4)),
	}

	if err := segment.CreateSegmentOnDisk(dir, vmeta, entries); err != nil {
		return fmt.Errorf("create segment on disk: %w", err)
	}

	if err := vb.boltStore.PutSegment(vmeta); err != nil {
		return fmt.Errorf("save segment meta: %w", err)
	}

	for _, e := range entries {
		rec := &vse.VectorRecord{
			ID:        e.ID,
			ExtID:     e.ExtID,
			Segment:   segID,
			Dimension: vb.dim,
			Bucket:    e.Bucket,
			ObjectKey: e.ObjectKey,
			Metadata:  e.Metadata,
			CreatedAt: e.CreatedAt,
		}
		if err := vb.boltStore.PutVector(rec); err != nil {
			return fmt.Errorf("save vector record: %w", err)
		}
	}

	if _, err := vb.segManager.LoadSegment(context.Background(), vmeta); err != nil {
		return fmt.Errorf("load segment: %w", err)
	}

	if err := vb.boltStore.SetIDGen(uint64(vb.idGen.Peek())); err != nil {
		return fmt.Errorf("save id gen: %w", err)
	}

	vb.nextSegID++
	vb.hotVecCount.Add(int64(len(entries)))
	vb.planner.UpdateSegments(
		vb.segManager.ListHotSegments(),
		vb.segManager.ListColdSegments(),
	)

	return nil
}

type segResult struct {
	results []vse.SearchResult
	segID   vse.SegmentID
}

func (vb *VSEBackend) Search(ctx context.Context, query Vector, topK int, filters map[string]string) ([]SearchResult, error) {
	start := time.Now()

	vb.mu.RLock()
	dim := vb.dim
	vb.mu.RUnlock()

	if len(query.Values) != dim {
		return nil, ErrInvalidDimension
	}
	if topK <= 0 {
		topK = 10
	}

	plan := vb.planner.Plan(ctx, query.Values, topK)

	merged := vb.mergePlanResults(plan, filters, topK)

	lat := time.Since(start)
	vb.queryCount.Add(1)
	vb.totalLatNs.Add(lat.Nanoseconds())
	vb.recordLatency(lat.Nanoseconds())

	return merged, nil
}

// recordLatency 记录延迟采样到环形缓冲区
func (vb *VSEBackend) recordLatency(ns int64) {
	vb.latencyMu.Lock()
	if vb.latencySamples == nil {
		vb.latencySamples = make([]int64, latencySampleSize)
	}
	vb.latencySamples[vb.latencyIdx%latencySampleSize] = ns
	vb.latencyIdx++
	vb.latencyMu.Unlock()
}

// percentile 计算当前延迟采样的百分位（p 为 0-100）
func (vb *VSEBackend) percentile(p float64) float64 {
	vb.latencyMu.Lock()
	if vb.latencySamples == nil || vb.latencyIdx == 0 {
		vb.latencyMu.Unlock()
		return 0
	}
	n := int(min(vb.latencyIdx, latencySampleSize))
	samples := make([]int64, n)
	copy(samples, vb.latencySamples[:n])
	vb.latencyMu.Unlock()

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	idx := int(float64(n-1) * p / 100.0)
	return float64(samples[idx]) / 1e6 // 转 ms
}

func (vb *VSEBackend) mergePlanResults(plan *planner.PlanResult, filters map[string]string, topK int) []SearchResult {
	type scored struct {
		result vse.SearchResult
		dist   float32
	}

	var all []scored
	for _, r := range plan.Results {
		all = append(all, scored{result: r, dist: r.Score})
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].dist < all[j].dist
	})
	if len(all) > topK {
		all = all[:topK]
	}

	seen := make(map[vse.VectorID]bool, len(all))
	out := make([]SearchResult, 0, len(all))

	for _, s := range all {
		if seen[s.result.ID] {
			continue
		}
		seen[s.result.ID] = true

		sr := SearchResult{Score: s.dist}
		rec, err := vb.boltStore.GetVector(s.result.ID)
		if err == nil && rec != nil {
			sr.ID = rec.ExtID
			sr.Bucket = rec.Bucket
			sr.ObjectKey = rec.ObjectKey
			sr.Metadata = rec.Metadata
		}

		if len(filters) > 0 {
			match := true
			for k, v := range filters {
				switch k {
				case "bucket":
					if sr.Bucket != v {
						match = false
					}
				case "object_key":
					if sr.ObjectKey != v {
						match = false
					}
				default:
					if sr.Metadata != nil {
						if mv, ok := sr.Metadata[k]; !ok || mv != v {
							match = false
						}
					} else {
						match = false
					}
				}
			}
			if !match {
				continue
			}
		}

		out = append(out, sr)
	}

	return out
}

func (vb *VSEBackend) Delete(_ context.Context, ids []string) error {
	vb.mu.Lock()
	defer vb.mu.Unlock()

	for _, extID := range ids {
		vb.boltStore.DeleteVectorByExtID(extID)
	}
	return nil
}

func (vb *VSEBackend) Build(ctx context.Context) error {
	vb.mu.Lock()
	defer vb.mu.Unlock()

	hotSegs := vb.segManager.ListHotSegments()
	if len(hotSegs) <= 1 {
		return nil
	}

	return vb.mergeSegments(ctx, hotSegs)
}

func (vb *VSEBackend) mergeSegments(ctx context.Context, segs []*segment.Segment) error {
	if len(segs) < 2 {
		return nil
	}

	targetID := vb.nextSegID
	vb.nextSegID++

	targetMeta := &vse.SegmentMeta{
		ID:   targetID,
		Tier: segs[0].Meta.Tier,
	}

	merged, err := vb.segManager.MergeSegments(ctx, segs, targetMeta, vb.metricFunc)
	if err != nil {
		return fmt.Errorf("merge segments: %w", err)
	}

	if err := vb.boltStore.PutSegment(merged.Meta); err != nil {
		return fmt.Errorf("save merged segment meta: %w", err)
	}

	for _, seg := range segs {
		seg.Close()
		if err := vb.boltStore.DeleteSegment(seg.Meta.ID); err != nil {
			return fmt.Errorf("delete old segment meta: %w", err)
		}
		os.RemoveAll(seg.Dir)
	}

	vb.planner.UpdateSegments(
		vb.segManager.ListHotSegments(),
		vb.segManager.ListColdSegments(),
	)

	return nil
}

func (vb *VSEBackend) GetStats() IndexStats {
	latencyMs := float64(0)
	qc := vb.queryCount.Load()
	if qc > 0 {
		latencyMs = float64(vb.totalLatNs.Load()) / float64(qc) / 1e6
	}

	memMB := float64(vb.hotVecCount.Load()) * float64(vb.dim) * 4 / 1024 / 1024

	return IndexStats{
		TotalVectors:  vb.hotVecCount.Load() + vb.coldVecCount.Load(),
		HotVectors:    vb.hotVecCount.Load(),
		ColdVectors:   vb.coldVecCount.Load(),
		MemoryUsageMB: memMB,
		IndexType:     "VSE",
		Dimension:     vb.dim,
		LastBuiltAt:   time.Now(),
		QueryCount:    qc,
		AvgLatencyMs:  latencyMs,
	}
}

func (vb *VSEBackend) mergeLoop() {
	interval := vb.config.MergePolicy.MergeInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			vb.tryMerge()
		case <-vb.stopped:
			return
		}
	}
}

func (vb *VSEBackend) tryMerge() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vb.mu.Lock()
	defer vb.mu.Unlock()

	hotSegs := vb.segManager.ListHotSegments()
	if candidates := vb.pickMergeCandidates(hotSegs, vb.config.MergePolicy.HotMaxSegments, vb.config.MergePolicy.HotTargetSize); len(candidates) >= 2 {
		vb.mergeSegments(ctx, candidates)
	}

	coldSegs := vb.segManager.ListColdSegments()
	if candidates := vb.pickMergeCandidates(coldSegs, vb.config.MergePolicy.ColdMaxSegments, vb.config.MergePolicy.ColdTargetSize); len(candidates) >= 2 {
		vb.mergeSegments(ctx, candidates)
	}
}

func (vb *VSEBackend) pickMergeCandidates(segs []*segment.Segment, maxSegments, targetSize int) []*segment.Segment {
	if len(segs) <= 1 {
		return nil
	}

	sort.Slice(segs, func(i, j int) bool {
		return segs[i].Meta.CreatedAt.Before(segs[j].Meta.CreatedAt)
	})

	trigger := false
	if len(segs) > maxSegments/2 {
		trigger = true
	}
	if !trigger {
		for _, seg := range segs {
			if seg.Meta.NumVectors >= int(float64(targetSize)*0.9) {
				trigger = true
				break
			}
		}
	}
	if !trigger {
		for _, seg := range segs {
			if seg.Meta.NumVectors < int(float64(targetSize)*0.2) {
				trigger = true
				break
			}
		}
	}
	if !trigger {
		return nil
	}

	sort.Slice(segs, func(i, j int) bool {
		return segs[i].Meta.NumVectors < segs[j].Meta.NumVectors
	})

	maxMerge := 3
	if len(segs) < maxMerge {
		maxMerge = len(segs)
	}
	return segs[:maxMerge]
}

func (vb *VSEBackend) warmup(ctx context.Context) {
	wp := vb.config.WarmupPolicy
	if !wp.Enabled {
		return
	}

	_ = ctx
	hotSegs := vb.segManager.ListHotSegments()
	_ = hotSegs

	if wp.PreloadCentroids {
		coldSegs := vb.segManager.ListColdSegments()
		for _, seg := range coldSegs {
			select {
			case <-vb.stopped:
				return
			default:
			}
			if seg.Meta.CentroidFile != "" {
				vb.blockCache.Set(fmt.Sprintf("centroid_%d", seg.Meta.ID), true)
			}
		}
	}
}

func (vb *VSEBackend) Close() error {
	close(vb.stopped)
	vb.prefetch.Stop()
	vb.segManager.Close()
	vb.boltStore.Close()
	return nil
}

var _ VectorIndex = (*VSEBackend)(nil)
