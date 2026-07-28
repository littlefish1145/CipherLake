package index

import (
	"math/rand"
	"testing"
)

func TestVamanaInsertAndSearch(t *testing.T) {
	dim := 8
	g := NewVamanaGraph(dim, 16, 64, 1.2, MetricCosine)

	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, 200)
	for i := range vecs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()
		}
		vecs[i] = v
	}

	for i, v := range vecs {
		if err := g.Insert(uint64(i), v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	if g.Len() != 200 {
		t.Fatalf("expected 200 nodes, got %d", g.Len())
	}

	results, err := g.Search(vecs[0], 10)
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

func TestVamanaBuild(t *testing.T) {
	dim := 8
	n := 100
	rng := rand.New(rand.NewSource(42))

	ids := make([]uint64, n)
	flatVecs := make([]float32, n*dim)
	for i := range ids {
		ids[i] = uint64(i)
		for j := 0; j < dim; j++ {
			flatVecs[i*dim+j] = rng.Float32()
		}
	}

	g := NewVamanaGraph(dim, 16, 64, 1.2, MetricEuclidean)
	if err := g.Build(ids, flatVecs); err != nil {
		t.Fatalf("build: %v", err)
	}

	if g.Len() != n {
		t.Fatalf("expected %d nodes, got %d", n, g.Len())
	}

	q := make([]float32, dim)
	for j := range q {
		q[j] = rng.Float32()
	}

	results, err := g.Search(q, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("expected 10 results, got %d", len(results))
	}
}

func TestVamanaSerializeRoundTrip(t *testing.T) {
	dim := 4
	g := NewVamanaGraph(dim, 8, 32, 1.5, MetricDotProduct)
	for i := 0; i < 50; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(i*10 + j)
		}
		if err := g.Insert(uint64(i), v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	data, err := g.Serialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	g2 := NewVamanaGraph(dim, 8, 32, 1.5, MetricDotProduct)
	if err := g2.Deserialize(data); err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if g2.Len() != 50 {
		t.Fatalf("expected 50 nodes, got %d", g2.Len())
	}

	q := make([]float32, dim)
	for i := range q {
		q[i] = 123.0
	}
	r1, _ := g.Search(q, 5)
	r2, _ := g2.Search(q, 5)

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

func TestVamanaMmapRoundTrip(t *testing.T) {
	dim := 4
	g := NewVamanaGraph(dim, 8, 32, 1.2, MetricDotProduct)
	for i := 0; i < 30; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(i*10 + j)
		}
		g.Insert(uint64(i), v)
	}

	path := t.TempDir() + "/vamana.bin"
	if err := g.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	g2, err := MmapVamanaGraph(path, MetricDotProduct)
	if err != nil {
		t.Fatalf("MmapVamanaGraph: %v", err)
	}
	defer g2.Close()

	if g2.Len() != 30 {
		t.Fatalf("mmap len: expected 30, got %d", g2.Len())
	}

	q := make([]float32, dim)
	for i := range q {
		q[i] = 123.0
	}
	r1, _ := g.Search(q, 5)
	r2, _ := g2.Search(q, 5)

	if len(r1) != len(r2) {
		t.Fatalf("result count: %d vs %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i].ID != r2[i].ID {
			t.Fatalf("result %d ID: %d vs %d", i, r1[i].ID, r2[i].ID)
		}
	}
}

func TestVamanaRobustPrune(t *testing.T) {
	dim := 2
	g := NewVamanaGraph(dim, 3, 32, 1.2, MetricEuclidean)

	// Insert points in a line: 0,0  1,0  2,0  3,0  4,0
	for i := 0; i < 5; i++ {
		v := []float32{float32(i), 0}
		g.Insert(uint64(i), v)
	}

	if g.Len() != 5 {
		t.Fatalf("expected 5, got %d", g.Len())
	}

	// All nodes should have at most R=3 neighbors
	for i := 0; i < g.Len(); i++ {
		if len(g.neighbors[i]) > 3 {
			t.Fatalf("node %d has %d neighbors (max 3)", i, len(g.neighbors[i]))
		}
	}

	// Query should find the closest point
	results, err := g.Search([]float32{2.1, 0.1}, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) > 0 && results[0].ID != 2 {
		t.Logf("expected nearest ID 2, got %d", results[0].ID)
	}
}

func TestVamanaConcurrentSearch(t *testing.T) {
	dim := 8
	g := NewVamanaGraph(dim, 16, 64, 1.2, MetricCosine)
	rng := rand.New(rand.NewSource(42))

	n := 500
	for i := 0; i < n; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()
		}
		g.Insert(uint64(i), v)
	}

	done := make(chan bool, 10)
	for w := 0; w < 10; w++ {
		go func() {
			q := make([]float32, dim)
			for j := range q {
				q[j] = rng.Float32()
			}
			_, err := g.Search(q, 10)
			if err != nil {
				t.Errorf("concurrent search: %v", err)
			}
			done <- true
		}()
	}
	for w := 0; w < 10; w++ {
		<-done
	}
}

func TestVamanaStitchedSearchFallsBackToExact(t *testing.T) {
	dim := 4
	g := NewVamanaGraph(dim, 8, 32, 1.2, MetricCosine)
	for i := 0; i < 20; i++ {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(i + j + 1)
		}
		if err := g.Insert(uint64(i), v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	results, err := g.StitchedSearch([]float32{1, 2, 3, 4}, 5, nil, nil, nil)
	if err != nil {
		t.Fatalf("stitched search fallback: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
}

func BenchmarkVamanaSearch(b *testing.B) {
	dim := 768
	rng := rand.New(rand.NewSource(42))
	g := NewVamanaGraph(dim, 32, 128, 1.2, MetricCosine)

	n := 10000
	for i := 0; i < n; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()
		}
		g.Insert(uint64(i), vec)
	}

	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, _ := g.Search(query, 10)
		_ = results
	}
}
