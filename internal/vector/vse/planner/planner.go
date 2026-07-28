package planner

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"cipherlake/internal/vector/vse"
	"cipherlake/internal/vector/vse/engine/segment"
)

type scoredResult struct {
	score float32
	id    uint64
	segID vse.SegmentID
}

type PlanResult struct {
	Results  []vse.SearchResult
	HotSegs  int
	ColdSegs int
	Searched int
	Latency  time.Duration
}

type Planner struct {
	mu             sync.RWMutex
	hotSegs        []*segment.Segment
	coldSegs       []*segment.Segment
	topK           int
	minHotResults  int
	searchWorkers  int
	totalQueries   atomic.Int64
	hotOnlyQueries atomic.Int64

	// scoredResult 切片池，避免每次查询 heap 分配
	scoredPool sync.Pool

	// 代价模型：hot/cold 延迟比采样
	hotLatencyNs   atomic.Int64 // 热段累计延迟（纳秒）
	hotQueryCount  atomic.Int64
	coldLatencyNs  atomic.Int64 // 冷段累计延迟（纳秒）
	coldQueryCount atomic.Int64

	// SegmentManager 引用，用于 Cold→Hot 提升回调
	segMgr *segment.SegmentManager
}

func NewPlanner(searchWorkers int) *Planner {
	if searchWorkers < 1 {
		searchWorkers = 1
	}
	return &Planner{
		minHotResults: 10,
		searchWorkers: searchWorkers,
		scoredPool: sync.Pool{New: func() any {
			return make([]scoredResult, 0, 256)
		}},
	}
}

// SetSegmentManager 设置 SegmentManager 引用，用于 Cold→Hot 提升回调
func (p *Planner) SetSegmentManager(sm *segment.SegmentManager) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.segMgr = sm
}

func (p *Planner) UpdateSegments(hot, cold []*segment.Segment) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hotSegs = hot
	p.coldSegs = cold
}

// QueryStats 返回 (totalQueries, hotOnlyQueries) 用于可观测性
func (p *Planner) QueryStats() (int64, int64) {
	return p.totalQueries.Load(), p.hotOnlyQueries.Load()
}

func (p *Planner) Plan(ctx context.Context, query []float32, topK int) *PlanResult {
	start := time.Now()

	p.mu.RLock()
	hotSegs := p.hotSegs
	coldSegs := p.coldSegs
	p.mu.RUnlock()

	if topK <= 0 {
		topK = 10
	}

	// 从池中获取 scoredResult 切片，避免 heap 分配
	results := p.scoredPool.Get().([]scoredResult)[:0]
	defer p.scoredPool.Put(results)

	hotResults := p.searchSegmentsInto(ctx, hotSegs, query, topK, results)
	searched := len(hotSegs)

	// 代价模型决策：是否需要搜索冷段
	// 条件1: 热段结果不足 topK 或 minHotResults
	// 条件2: 代价模型评估——如果冷段延迟远高于热段，且热段结果已足够，跳过冷段
	needCold := len(hotResults) < topK || len(hotResults) < p.minHotResults
	if needCold && p.shouldSkipCold(topK, len(hotResults)) {
		needCold = false
	}

	if needCold {
		more := p.searchSegmentsInto(ctx, coldSegs, query, topK-len(hotResults), hotResults[len(hotResults):])
		hotResults = hotResults[:len(hotResults)+len(more)]
		searched += len(coldSegs)
	}

	merged := mergeScored(hotResults, topK)

	lat := time.Since(start)

	p.totalQueries.Add(1)
	if searched <= len(hotSegs) {
		p.hotOnlyQueries.Add(1)
	}

	return &PlanResult{
		Results:  merged,
		HotSegs:  len(hotSegs),
		ColdSegs: len(coldSegs),
		Searched: searched,
		Latency:  lat,
	}
}

// shouldSkipCold 基于代价模型决定是否跳过冷段搜索
// 当热段结果充足且冷段延迟显著高于热段时，跳过冷段以降低 P99
func (p *Planner) shouldSkipCold(topK, hotResultCount int) bool {
	if hotResultCount < topK {
		return false
	}
	hotNs := p.hotLatencyNs.Load()
	hotQ := p.hotQueryCount.Load()
	coldNs := p.coldLatencyNs.Load()
	coldQ := p.coldQueryCount.Load()
	// 样本不足时无法评估，保守搜索
	if coldQ < 10 || hotQ < 1 || hotNs == 0 {
		return false
	}
	// 计算平均延迟比
	hotAvg := hotNs / hotQ
	coldAvg := coldNs / coldQ
	// 冷段延迟超过热段 5 倍且热段结果已 >= topK 时跳过
	ratio := float64(coldAvg) / float64(max(1, hotAvg))
	return ratio > 5.0
}

// searchSegmentsInto 将结果追加到 dst 并返回追加后的切片
func (p *Planner) searchSegmentsInto(ctx context.Context, segs []*segment.Segment, query []float32, topK int, dst []scoredResult) []scoredResult {
	if len(segs) == 0 || topK <= 0 {
		return dst
	}

	workers := p.searchWorkers
	if workers > len(segs) {
		workers = len(segs)
	}

	type segResult struct {
		idx int
		sr  []vse.SearchResult
	}

	ch := make(chan segResult, len(segs))
	sem := make(chan struct{}, workers)

	for i, seg := range segs {
		select {
		case <-ctx.Done():
			return dst
		default:
		}

		sem <- struct{}{}
		go func(idx int, s *segment.Segment) {
			defer func() { <-sem }()
			segStart := time.Now()
			sr, err := s.Search(query, topK)
			if err != nil {
				ch <- segResult{idx: idx}
				return
			}
			segLat := time.Since(segStart).Nanoseconds()
			// 记录延迟采样，用于代价模型
			if s.Meta.Tier == vse.TierCold || s.Meta.Tier == vse.TierDiskANN {
				p.coldLatencyNs.Add(segLat)
				p.coldQueryCount.Add(1)
				// 记录冷段访问，用于 Cold→Hot 自动提升
				if p.segMgr != nil {
					p.segMgr.RecordColdAccess(s.Meta.ID)
				}
			} else {
				p.hotLatencyNs.Add(segLat)
				p.hotQueryCount.Add(1)
			}
			ch <- segResult{idx: idx, sr: sr}
		}(i, seg)
	}

	for range segs {
		r := <-ch
		for _, sr := range r.sr {
			dst = append(dst, scoredResult{
				score: sr.Score,
				id:    uint64(sr.ID),
				segID: sr.SegmentID,
			})
		}
	}

	return dst
}

func mergeScored(src []scoredResult, topK int) []vse.SearchResult {
	if len(src) == 0 {
		return nil
	}

	sort.Slice(src, func(i, j int) bool {
		return src[i].score < src[j].score
	})

	if len(src) > topK {
		src = src[:topK]
	}

	seen := make(map[uint64]bool, len(src))
	out := make([]vse.SearchResult, 0, len(src))

	for _, s := range src {
		if seen[s.id] {
			continue
		}
		seen[s.id] = true
		out = append(out, vse.SearchResult{
			ID:        vse.VectorID(s.id),
			Score:     s.score,
			SegmentID: s.segID,
		})
	}

	if len(out) > topK {
		out = out[:topK]
	}
	return out
}
