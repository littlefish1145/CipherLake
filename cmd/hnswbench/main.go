package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"nexus/internal/vector/vse/engine/index"
	"nexus/internal/vector/vse/simd"
)

type benchmarkEntry struct {
	EfSearch   int     `json:"ef_search"`
	RecallAt10 float64 `json:"recall_at_10"`
	QPS        float64 `json:"qps"`
	P50Us      float64 `json:"p50_us"`
	P95Us      float64 `json:"p95_us"`
	AvgUs      float64 `json:"avg_us"`
}

type comparisonEntry struct {
	RecallAt10  float64 `json:"recall_at_10"`
	CurrentQPS  float64 `json:"current_qps"`
	BaselineQPS float64 `json:"baseline_qps"`
	Ratio       float64 `json:"ratio"`
}

type benchmarkReport struct {
	Dataset          string            `json:"dataset"`
	Metric           string            `json:"metric"`
	Dimension        int               `json:"dimension"`
	BaseCount        int               `json:"base_count"`
	QueryCount       int               `json:"query_count"`
	TopK             int               `json:"topk"`
	M                int               `json:"m"`
	EfConstruction   int               `json:"ef_construction"`
	Threads          int               `json:"threads"`
	Warmup           int               `json:"warmup"`
	CPUISA           string            `json:"cpu_isa"`
	BuildSeconds     float64           `json:"build_seconds"`
	MemoryBytes      uint64            `json:"memory_bytes"`
	Entries          []benchmarkEntry  `json:"entries"`
	Comparisons      []comparisonEntry `json:"comparisons,omitempty"`
	AcceptanceRatio  float64           `json:"acceptance_ratio,omitempty"`
	AcceptanceRecall float64           `json:"acceptance_recall_at_10,omitempty"`
	Passed           bool              `json:"passed,omitempty"`
	ComparedAgainst  string            `json:"compared_against,omitempty"`
}

func main() {
	dataDir := flag.String("data-dir", "data", "directory containing sift_base.fvecs, sift_query.fvecs, sift_groundtruth.ivecs")
	m := flag.Int("m", 16, "HNSW M parameter")
	efConstruction := flag.Int("ef-construction", 200, "HNSW efConstruction parameter")
	topK := flag.Int("topk", 10, "topK for evaluation")
	queryCount := flag.Int("queries", 100, "number of SIFT1M queries to evaluate")
	threads := flag.Int("threads", runtime.NumCPU(), "GOMAXPROCS used during benchmark")
	warmup := flag.Int("warmup", 16, "number of warmup queries before measurement")
	metric := flag.String("metric", "l2", "distance metric: l2, cosine, dot")
	efSweep := flag.String("ef-sweep", "64,96,128,160,192,256", "comma-separated efSearch values")
	baselineFile := flag.String("baseline", "", "optional baseline JSON report (for example FAISS results)")
	outputFile := flag.String("output", "", "optional output JSON file")
	indexFile := flag.String("index", "", "path to save/load serialized index (avoids rebuild)")
	flag.Parse()

	runtime.GOMAXPROCS(*threads)
	simd.Init()

	queryVecs, dim, err := loadFvecs(*dataDir + "/sift_query.fvecs")
	must(err)
	gt, err := loadIvecs(*dataDir + "/sift_groundtruth.ivecs")
	must(err)

	if *queryCount > len(gt) {
		*queryCount = len(gt)
	}
	queryVecs = queryVecs[:(*queryCount)*dim]
	gt = gt[:*queryCount]

	mt := parseMetric(*metric)

	var h *index.FlatHNSW
	var buildSeconds float64
	var memoryBytes uint64

	if *indexFile != "" {
		if _, err := os.Stat(*indexFile); err == nil {
			h, err = index.MmapFlatHNSW(*indexFile, mt)
			must(err)
		} else {
			baseVecs, _, err := loadFvecs(*dataDir + "/sift_base.fvecs")
			must(err)
			baseNv := len(baseVecs) / dim
			ids := make([]uint64, baseNv)
			for i := range ids {
				ids[i] = uint64(i + 1)
			}

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			h = index.NewFlatHNSW(dim, *m, *efConstruction, 1.0/math.Log(float64(*m)), mt)
			buildStart := time.Now()
			must(h.Build(ids, baseVecs))
			buildSeconds = time.Since(buildStart).Seconds()
			runtime.GC()
			runtime.ReadMemStats(&after)
			if after.HeapAlloc >= before.HeapAlloc {
				memoryBytes = after.HeapAlloc - before.HeapAlloc
			}
			must(h.WriteFile(*indexFile))
		}
	} else {
		baseVecs, _, err := loadFvecs(*dataDir + "/sift_base.fvecs")
		must(err)
		baseNv := len(baseVecs) / dim
		ids := make([]uint64, baseNv)
		for i := range ids {
			ids[i] = uint64(i + 1)
		}

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		h = index.NewFlatHNSW(dim, *m, *efConstruction, 1.0/math.Log(float64(*m)), mt)
		buildStart := time.Now()
		must(h.Build(ids, baseVecs))
		buildSeconds = time.Since(buildStart).Seconds()
		runtime.GC()
		runtime.ReadMemStats(&after)
		if after.HeapAlloc >= before.HeapAlloc {
			memoryBytes = after.HeapAlloc - before.HeapAlloc
		}
	}

	efs := parseSweep(*efSweep)
	report := benchmarkReport{
		Dataset:        "SIFT1M",
		Metric:         *metric,
		Dimension:      dim,
		BaseCount:      h.Len(),
		QueryCount:     *queryCount,
		TopK:           *topK,
		M:              *m,
		EfConstruction: *efConstruction,
		Threads:        *threads,
		Warmup:         *warmup,
		CPUISA:         simd.RuntimeInfo.String(),
		BuildSeconds:   buildSeconds,
		MemoryBytes:    memoryBytes,
		Entries:        make([]benchmarkEntry, 0, len(efs)),
	}

	for _, ef := range efs {
		h.EfSearch = ef
		runWarmup(h, queryVecs, dim, *warmup, *topK)
		latencies := make([]float64, 0, *queryCount)
		recalled := 0
		total := 0

		start := time.Now()
		for i := 0; i < *queryCount; i++ {
			query := queryVecs[i*dim : (i+1)*dim]
			qs := time.Now()
			results, err := h.Search(query, *topK)
			must(err)
			latencies = append(latencies, float64(time.Since(qs).Microseconds()))

			resultIDs := make([]uint64, len(results))
			for j, r := range results {
				resultIDs[j] = r.ID
			}
			hit, cnt := computeRecall(resultIDs, gt[i], *topK)
			recalled += hit
			total += cnt
		}
		totalTime := time.Since(start)
		qps := float64(*queryCount) / totalTime.Seconds()
		recall := float64(recalled) / float64(total)

		report.Entries = append(report.Entries, benchmarkEntry{
			EfSearch:   ef,
			RecallAt10: recall,
			QPS:        qps,
			P50Us:      percentile(latencies, 50),
			P95Us:      percentile(latencies, 95),
			AvgUs:      avg(latencies),
		})
	}

	if *baselineFile != "" {
		base, err := readBaseline(*baselineFile)
		must(err)
		comparisons, ratio, recall, passed := compareAgainstBaseline(report.Entries, base.Entries)
		report.Comparisons = comparisons
		report.AcceptanceRatio = ratio
		report.AcceptanceRecall = recall
		report.Passed = passed
		report.ComparedAgainst = *baselineFile
	}

	writeReport(report, *outputFile)
}

func runWarmup(h *index.FlatHNSW, queryVecs []float32, dim, warmup, topK int) {
	if warmup <= 0 || len(queryVecs) == 0 {
		return
	}
	if warmup > len(queryVecs)/dim {
		warmup = len(queryVecs) / dim
	}
	for i := 0; i < warmup; i++ {
		query := queryVecs[i*dim : (i+1)*dim]
		_, _ = h.Search(query, topK)
	}
}

func parseMetric(metric string) index.MetricType {
	switch strings.ToLower(metric) {
	case "dot":
		return index.MetricDotProduct
	case "cosine":
		return index.MetricCosine
	default:
		return index.MetricEuclidean
	}
}

func loadFvecs(path string) ([]float32, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(data) < 4 {
		return nil, 0, fmt.Errorf("file too small: %d bytes", len(data))
	}
	dim := int(binary.LittleEndian.Uint32(data[:4]))
	vecSize := 4 + dim*4
	n := len(data) / vecSize
	if n*vecSize != len(data) {
		return nil, 0, fmt.Errorf("file size %d not aligned to vec size %d", len(data), vecSize)
	}
	vecs := make([]float32, n*dim)
	for i := 0; i < n; i++ {
		off := i*vecSize + 4
		for j := 0; j < dim; j++ {
			vecs[i*dim+j] = math.Float32frombits(binary.LittleEndian.Uint32(data[off+j*4:]))
		}
	}
	return vecs, dim, nil
}

func loadIvecs(path string) ([][]int32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, fmt.Errorf("file too small: %d bytes", len(data))
	}
	dim := int(binary.LittleEndian.Uint32(data[:4]))
	vecSize := 4 + dim*4
	n := len(data) / vecSize
	if n*vecSize != len(data) {
		return nil, fmt.Errorf("file size %d not aligned to vec size %d", len(data), vecSize)
	}
	result := make([][]int32, n)
	for i := 0; i < n; i++ {
		off := i*vecSize + 4
		row := make([]int32, dim)
		for j := 0; j < dim; j++ {
			row[j] = int32(binary.LittleEndian.Uint32(data[off+j*4:]))
		}
		result[i] = row
	}
	return result, nil
}

func computeRecall(results []uint64, gt []int32, k int) (hit, total int) {
	hitSet := make(map[uint64]struct{}, len(results))
	for _, id := range results {
		hitSet[id] = struct{}{}
	}
	for i := 0; i < k && i < len(gt); i++ {
		total++
		if _, ok := hitSet[uint64(gt[i]+1)]; ok {
			hit++
		}
	}
	return hit, total
}

func parseSweep(s string) []int {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && v > 0 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = append(out, 64, 128, 256)
	}
	return out
}

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	idx := int(float64(len(cp)-1) * p / 100.0)
	return cp[idx]
}

func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func readBaseline(path string) (benchmarkReport, error) {
	var report benchmarkReport
	data, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	err = json.Unmarshal(data, &report)
	return report, err
}

func compareAgainstBaseline(current, baseline []benchmarkEntry) ([]comparisonEntry, float64, float64, bool) {
	if len(current) == 0 || len(baseline) == 0 {
		return nil, 0, 0, false
	}
	comparisons := make([]comparisonEntry, 0, len(current))
	bestRatio := 0.0
	bestRecall := 0.0
	passed := false
	for _, cur := range current {
		match := nearestRecall(cur.RecallAt10, baseline)
		if match == nil || match.QPS == 0 {
			continue
		}
		ratio := cur.QPS / match.QPS
		comparisons = append(comparisons, comparisonEntry{
			RecallAt10:  cur.RecallAt10,
			CurrentQPS:  cur.QPS,
			BaselineQPS: match.QPS,
			Ratio:       ratio,
		})
		if ratio > bestRatio {
			bestRatio = ratio
			bestRecall = cur.RecallAt10
		}
		if ratio >= 0.97 {
			passed = true
		}
	}
	sort.Slice(comparisons, func(i, j int) bool {
		return comparisons[i].RecallAt10 < comparisons[j].RecallAt10
	})
	return comparisons, bestRatio, bestRecall, passed
}

func nearestRecall(target float64, entries []benchmarkEntry) *benchmarkEntry {
	if len(entries) == 0 {
		return nil
	}
	best := &entries[0]
	bestGap := math.Abs(entries[0].RecallAt10 - target)
	for i := 1; i < len(entries); i++ {
		gap := math.Abs(entries[i].RecallAt10 - target)
		if gap < bestGap {
			best = &entries[i]
			bestGap = gap
		}
	}
	return best
}

func writeReport(report benchmarkReport, outputFile string) {
	data, err := json.MarshalIndent(report, "", "  ")
	must(err)
	if outputFile != "" {
		must(os.WriteFile(outputFile, data, 0644))
	}
	fmt.Println(string(data))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
