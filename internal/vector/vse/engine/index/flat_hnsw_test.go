package index

import (
	"math/rand"
	"testing"
)

func TestFlatHNSWMmapRoundTrip(t *testing.T) {
	dim := 4
	h := NewFlatHNSW(dim, 8, 32, 0.5, MetricDotProduct)
	for i := 0; i < 30; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(i*10 + j)
		}
		h.Insert(uint64(i), v)
	}

	path := t.TempDir() + "/graph.bin"
	if err := h.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h2, err := MmapFlatHNSW(path, MetricDotProduct)
	if err != nil {
		t.Fatalf("MmapFlatHNSW: %v", err)
	}
	defer h2.Close()

	if h2.Len() != 30 {
		t.Fatalf("mmap len: expected 30, got %d", h2.Len())
	}

	q := make([]float32, dim)
	for i := range q {
		q[i] = 123.0
	}
	r1, _ := h.Search(q, 5)
	r2, _ := h2.Search(q, 5)

	if len(r1) != len(r2) {
		t.Fatalf("result count: %d vs %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i].ID != r2[i].ID {
			t.Fatalf("result %d ID: %d vs %d", i, r1[i].ID, r2[i].ID)
		}
		if r1[i].Score != r2[i].Score {
			t.Fatalf("result %d Score: %f vs %f", i, r1[i].Score, r2[i].Score)
		}
	}
}

func TestFlatHNSWMmapSearchZeroAlloc(t *testing.T) {
	dim := 8
	h := NewFlatHNSW(dim, 16, 64, 0.5, MetricCosine)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 500; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()
		}
		h.Insert(uint64(i), v)
	}

	path := t.TempDir() + "/graph.bin"
	if err := h.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h2, err := MmapFlatHNSW(path, MetricCosine)
	if err != nil {
		t.Fatalf("MmapFlatHNSW: %v", err)
	}
	defer h2.Close()

	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32()
	}

	results, err := h2.Search(query, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Score == 0 {
		t.Fatal("Score should not be 0")
	}
}

func BenchmarkFlatHNSWSearch(b *testing.B) {
	dim := 768
	rng := rand.New(rand.NewSource(42))
	h := NewFlatHNSW(dim, 16, 64, 0.5, MetricCosine)

	n := 10000
	for i := 0; i < n; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()
		}
		h.Insert(uint64(i), vec)
	}

	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, _ := h.Search(query, 10)
		_ = results
	}
}

func TestFlatHNSWInsertAndSearch(t *testing.T) {
	dim := 8
	h := NewFlatHNSW(dim, 16, 64, 0.5, MetricCosine)

	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, 100)
	for i := range vecs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()
		}
		vecs[i] = v
	}

	for i, v := range vecs {
		if err := h.Insert(uint64(i), v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	if h.Len() != 100 {
		t.Fatalf("expected 100 nodes, got %d", h.Len())
	}

	results, err := h.Search(vecs[0], 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 result")
	}

	if results[0].ID != 0 {
		t.Fatalf("expected nearest neighbor ID 0, got %d", results[0].ID)
	}
}

func TestFlatHNSWSerializeRoundTrip(t *testing.T) {
	dim := 4
	h := NewFlatHNSW(dim, 8, 32, 0.5, MetricDotProduct)
	for i := 0; i < 50; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(i*10 + j)
		}
		if err := h.Insert(uint64(i), v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	data, err := h.Serialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	h2 := NewFlatHNSW(dim, 8, 32, 0.5, MetricDotProduct)
	if err := h2.Deserialize(data); err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if h2.Len() != 50 {
		t.Fatalf("expected 50 nodes after deserialize, got %d", h2.Len())
	}

	q := make([]float32, dim)
	for i := range q {
		q[i] = 123.0
	}
	r1, _ := h.Search(q, 5)
	r2, _ := h2.Search(q, 5)

	if len(r1) != len(r2) {
		t.Fatalf("result count mismatch: %d vs %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i].ID != r2[i].ID {
			t.Fatalf("result %d ID mismatch: %d vs %d", i, r1[i].ID, r2[i].ID)
		}
		if r1[i].Score != r2[i].Score {
			t.Fatalf("result %d Score mismatch: %f vs %f", i, r1[i].Score, r2[i].Score)
		}
	}
}

func TestFlatHNSWSearchReturnsScores(t *testing.T) {
	dim := 4
	h := NewFlatHNSW(dim, 8, 32, 0.5, MetricDotProduct)
	for i := 0; i < 20; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(i + j)
		}
		h.Insert(uint64(i), v)
	}

	results, err := h.Search([]float32{1, 2, 3, 4}, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected results")
	}

	for i, r := range results {
		if r.Score == 0 && r.ID != 0 {
			t.Fatalf("result %d has Score=0 for ID=%d (Score=%f)", i, r.ID, r.Score)
		}
	}
}

func TestFlatHNSWSearchWithStats(t *testing.T) {
	dim := 8
	h := NewFlatHNSW(dim, 16, 64, 0.5, MetricCosine)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()
		}
		if err := h.Insert(uint64(i), v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	query := make([]float32, dim)
	for i := range query {
		query[i] = rng.Float32()
	}

	results, stats, err := h.SearchWithStats(query, 10)
	if err != nil {
		t.Fatalf("search with stats: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if stats.DistanceCalls == 0 {
		t.Fatal("expected distance calls to be recorded")
	}
	if stats.CandidatePops == 0 {
		t.Fatal("expected candidate pops to be recorded")
	}
}

func TestFlatHNSWSearchAllocs(t *testing.T) {
	dim := 8
	h := NewFlatHNSW(dim, 16, 64, 0.5, MetricCosine)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 500; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()
		}
		if err := h.Insert(uint64(i), v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	query := make([]float32, dim)
	for i := range query {
		query[i] = rng.Float32()
	}

	allocs := testing.AllocsPerRun(100, func() {
		results, err := h.Search(query, 10)
		if err != nil || len(results) == 0 {
			t.Fatalf("search failed: %v", err)
		}
	})
	if allocs > 0 {
		t.Fatalf("expected zero alloc search path, got %.2f allocs/run", allocs)
	}
}
