package quantizer

import (
	"fmt"
	"math"
	"sort"
)

type Quantizer interface {
	Encode(vec []float32) ([]byte, error)
	Decode(data []byte) ([]float32, error)
	CodeSize() int
	Type() string
}

type SQQuantizer struct {
	dim     int
	min     []float32
	max     []float32
	trained bool
}

func NewSQQuantizer(dim int) *SQQuantizer {
	return &SQQuantizer{
		dim: dim,
		min: make([]float32, dim),
		max: make([]float32, dim),
	}
}

func (q *SQQuantizer) Train(vectors [][]float32) error {
	if len(vectors) == 0 {
		return fmt.Errorf("no training vectors")
	}
	dim := len(vectors[0])
	if dim == 0 {
		return fmt.Errorf("zero dimension")
	}

	for d := 0; d < dim; d++ {
		q.min[d] = vectors[0][d]
		q.max[d] = vectors[0][d]
	}
	for _, v := range vectors[1:] {
		if len(v) != dim {
			return fmt.Errorf("inconsistent dimension: expected %d, got %d", dim, len(v))
		}
		for d := 0; d < dim; d++ {
			if v[d] < q.min[d] {
				q.min[d] = v[d]
			}
			if v[d] > q.max[d] {
				q.max[d] = v[d]
			}
		}
	}
	q.trained = true
	return nil
}

func (q *SQQuantizer) Encode(vec []float32) ([]byte, error) {
	if !q.trained {
		return nil, fmt.Errorf("quantizer not trained")
	}
	if len(vec) != q.dim {
		return nil, fmt.Errorf("dimension mismatch: %d vs %d", len(vec), q.dim)
	}
	data := make([]byte, q.dim)
	for d := 0; d < q.dim; d++ {
		span := q.max[d] - q.min[d]
		if span == 0 {
			data[d] = 0
		} else {
			scaled := (vec[d] - q.min[d]) / span
			if scaled < 0 {
				scaled = 0
			}
			if scaled > 1.0 {
				scaled = 1.0
			}
			data[d] = byte(scaled * 255.0)
		}
	}
	return data, nil
}

func (q *SQQuantizer) Decode(data []byte) ([]float32, error) {
	if !q.trained {
		return nil, fmt.Errorf("quantizer not trained")
	}
	if len(data) != q.dim {
		return nil, fmt.Errorf("data size mismatch: %d vs %d", len(data), q.dim)
	}
	vec := make([]float32, q.dim)
	for d := 0; d < q.dim; d++ {
		ratio := float32(data[d]) / 255.0
		vec[d] = q.min[d] + ratio*(q.max[d]-q.min[d])
	}
	return vec, nil
}

func (q *SQQuantizer) CodeSize() int { return q.dim }
func (q *SQQuantizer) Type() string  { return "SQ8" }

type PQQuantizer struct {
	dim        int
	subVecs    int
	subDim     int
	nbits      int
	ks         int
	centroids  [][][]float32
	trained    bool
}

func NewPQQuantizer(dim, subVecs, nbits int) *PQQuantizer {
	if nbits > 8 {
		nbits = 8
	}
	ks := 1 << nbits
	if subVecs <= 0 {
		subVecs = dim / 8
		if subVecs < 1 {
			subVecs = 1
		}
	}
	subDim := dim / subVecs
	if subDim*subVecs < dim {
		subVecs = dim / subDim
		if subVecs < 1 {
			subVecs = 1
		}
	}

	return &PQQuantizer{
		dim:       dim,
		subVecs:   subVecs,
		subDim:    subDim,
		nbits:     nbits,
		ks:        ks,
		centroids: make([][][]float32, subVecs),
	}
}

func (q *PQQuantizer) Train(vectors [][]float32) error {
	if len(vectors) == 0 {
		return fmt.Errorf("no training vectors")
	}
	n := len(vectors)

	for s := 0; s < q.subVecs; s++ {
		start := s * q.subDim
		end := start + q.subDim
		if s == q.subVecs-1 {
			end = q.dim
		}
		subVectors := make([][]float32, n)
		for i, v := range vectors {
			subVectors[i] = v[start:end]
		}

		centroids := q.kmeans(subVectors, q.ks)
		q.centroids[s] = centroids
	}

	q.trained = true
	return nil
}

func (q *PQQuantizer) kmeans(data [][]float32, k int) [][]float32 {
	n := len(data)
	if n == 0 || len(data[0]) == 0 {
		return nil
	}
	dim := len(data[0])

	if k > n {
		k = n
	}

	centroids := make([][]float32, k)
	for i := 0; i < k; i++ {
		idx := i * n / k
		if idx >= n {
			idx = n - 1
		}
		centroids[i] = make([]float32, dim)
		copy(centroids[i], data[idx])
	}

	assignments := make([]int, n)
	for iter := 0; iter < 20; iter++ {
		changed := false
		for i, vec := range data {
			bestD := float32(math.MaxFloat32)
			bestK := 0
			for j, c := range centroids {
				d := float32(0)
				for d2 := 0; d2 < dim; d2++ {
					diff := vec[d2] - c[d2]
					d += diff * diff
				}
				if d < bestD {
					bestD = d
					bestK = j
				}
			}
			if assignments[i] != bestK {
				assignments[i] = bestK
				changed = true
			}
		}
		if !changed {
			break
		}

		newCentroids := make([][]float32, k)
		counts := make([]int, k)
		for i := 0; i < k; i++ {
			newCentroids[i] = make([]float32, dim)
		}
		for i, vec := range data {
			ci := assignments[i]
			for d := 0; d < dim; d++ {
				newCentroids[ci][d] += vec[d]
			}
			counts[ci]++
		}
		for i := 0; i < k; i++ {
			if counts[i] > 0 {
				for d := 0; d < dim; d++ {
					newCentroids[i][d] /= float32(counts[i])
				}
			} else {
				copy(newCentroids[i], centroids[i])
			}
		}
		centroids = newCentroids
	}

	return centroids
}

func (q *PQQuantizer) Trained() bool {
	return q.trained
}

func (q *PQQuantizer) Encode(vec []float32) ([]byte, error) {
	if !q.trained {
		return nil, fmt.Errorf("quantizer not trained")
	}
	if len(vec) != q.dim {
		return nil, fmt.Errorf("dimension mismatch: %d vs %d", len(vec), q.dim)
	}

	data := make([]byte, q.subVecs)
	for s := 0; s < q.subVecs; s++ {
		start := s * q.subDim
		end := start + q.subDim
		if s == q.subVecs-1 {
			end = q.dim
		}
		subVec := vec[start:end]

		bestD := float32(math.MaxFloat32)
		bestK := 0
		for j, c := range q.centroids[s] {
			d := float32(0)
			for di := 0; di < len(subVec); di++ {
				diff := subVec[di] - c[di]
				d += diff * diff
			}
			if d < bestD {
				bestD = d
				bestK = j
			}
		}
		data[s] = byte(bestK)
	}
	return data, nil
}

func (q *PQQuantizer) Decode(data []byte) ([]float32, error) {
	if !q.trained {
		return nil, fmt.Errorf("quantizer not trained")
	}
	if len(data) != q.subVecs {
		return nil, fmt.Errorf("data size mismatch: %d vs %d", len(data), q.subVecs)
	}

	vec := make([]float32, q.dim)
	for s := 0; s < q.subVecs; s++ {
		start := s * q.subDim
		end := start + q.subDim
		if s == q.subVecs-1 {
			end = q.dim
		}
		ci := int(data[s])
		if ci >= len(q.centroids[s]) {
			ci = 0
		}
		copy(vec[start:end], q.centroids[s][ci][:end-start])
	}
	return vec, nil
}

func (q *PQQuantizer) CodeSize() int { return q.subVecs }
func (q *PQQuantizer) Type() string  { return fmt.Sprintf("PQ_%dx%dbits", q.subVecs, q.nbits) }
func (q *PQQuantizer) SubVecs() int  { return q.subVecs }
func (q *PQQuantizer) SubDim() int   { return q.subDim }
func (q *PQQuantizer) Centroids() [][][]float32 { return q.centroids }

func (q *PQQuantizer) DistancePQ(codes []byte, vec []float32) float32 {
	var dist float32
	for s := 0; s < q.subVecs; s++ {
		start := s * q.subDim
		end := start + q.subDim
		if s == q.subVecs-1 {
			end = q.dim
		}
		ci := int(codes[s])
		if ci >= len(q.centroids[s]) {
			ci = 0
		}
		c := q.centroids[s][ci]
		for di := 0; di < end-start; di++ {
			diff := vec[start+di] - c[di]
			dist += diff * diff
		}
	}
	return dist
}

func (q *PQQuantizer) CentroidDist(subIdx int, code byte, subVec []float32) float32 {
	ci := int(code)
	if ci >= len(q.centroids[subIdx]) {
		ci = 0
	}
	c := q.centroids[subIdx][ci]
	var dist float32
	for i := 0; i < len(subVec); i++ {
		diff := subVec[i] - c[i]
		dist += diff * diff
	}
	return dist
}

func (q *PQQuantizer) PrecomputeQueryDistances(vec []float32) [][]float32 {
	table := make([][]float32, q.subVecs)
	for s := 0; s < q.subVecs; s++ {
		start := s * q.subDim
		end := start + q.subDim
		if s == q.subVecs-1 {
			end = q.dim
		}
		subVec := vec[start:end]
		table[s] = make([]float32, q.ks)
		for j := 0; j < q.ks; j++ {
			table[s][j] = 0
			c := q.centroids[s][j]
			for di := 0; di < end-start; di++ {
				diff := subVec[di] - c[di]
				table[s][j] += diff * diff
			}
		}
	}
	return table
}

// TrainFromFlat 从扁平 float32 数组训练 PQ
func (q *PQQuantizer) TrainFromFlat(flatVecs []float32, n int) error {
	if n == 0 || len(flatVecs) < n*q.dim {
		return fmt.Errorf("insufficient training data: %d vectors, need %d floats", n, n*q.dim)
	}
	vecs := make([][]float32, n)
	for i := 0; i < n; i++ {
		vecs[i] = flatVecs[i*q.dim : (i+1)*q.dim]
	}
	return q.Train(vecs)
}

// SetCentroids 直接设置 PQ 质心（反序列化时使用）
func (q *PQQuantizer) SetCentroids(c [][][]float32) {
	q.centroids = c
	q.trained = true
}

func (q *PQQuantizer) DistanceADC(table [][]float32, codes []byte) float32 {
	var dist float32
	for s := 0; s < q.subVecs; s++ {
		dist += table[s][codes[s]]
	}
	return dist
}

type IVF struct {
	Centroids [][]float32
	Lists     [][]int
	Dim       int
	Ncentroids int
	trained   bool
}

func NewIVF(dim, ncentroids int) *IVF {
	return &IVF{
		Centroids:  make([][]float32, ncentroids),
		Lists:      make([][]int, ncentroids),
		Dim:        dim,
		Ncentroids: ncentroids,
	}
}

func (ivf *IVF) Train(vectors [][]float32) error {
	n := len(vectors)
	if n == 0 {
		return fmt.Errorf("no training vectors")
	}
	k := ivf.Ncentroids
	if k > n {
		k = n
	}

	ivf.Centroids = make([][]float32, k)
	for i := 0; i < k; i++ {
		idx := i * n / k
		if idx >= n {
			idx = n - 1
		}
		ivf.Centroids[i] = make([]float32, ivf.Dim)
		copy(ivf.Centroids[i], vectors[idx])
	}

	assignments := make([]int, n)
	for iter := 0; iter < 20; iter++ {
		changed := false
		for i, vec := range vectors {
			bestD := float32(math.MaxFloat32)
			bestK := 0
			for j, c := range ivf.Centroids {
				d := float32(0)
				for di := 0; di < ivf.Dim; di++ {
					diff := vec[di] - c[di]
					d += diff * diff
				}
				if d < bestD {
					bestD = d
					bestK = j
				}
			}
			if assignments[i] != bestK {
				assignments[i] = bestK
				changed = true
			}
		}
		if !changed {
			break
		}

		newCentroids := make([][]float32, k)
		counts := make([]int, k)
		for i := 0; i < k; i++ {
			newCentroids[i] = make([]float32, ivf.Dim)
		}
		for i, vec := range vectors {
			ci := assignments[i]
			for d := 0; d < ivf.Dim; d++ {
				newCentroids[ci][d] += vec[d]
			}
			counts[ci]++
		}
		for i := 0; i < k; i++ {
			if counts[i] > 0 {
				for d := 0; d < ivf.Dim; d++ {
					newCentroids[i][d] /= float32(counts[i])
				}
			} else {
				copy(newCentroids[i], ivf.Centroids[i])
			}
		}
		ivf.Centroids = newCentroids
	}

	ivf.Lists = make([][]int, k)
	for i, vec := range vectors {
		bestD := float32(math.MaxFloat32)
		bestK := 0
		for j, c := range ivf.Centroids {
			d := float32(0)
			for di := 0; di < ivf.Dim; di++ {
				diff := vec[di] - c[di]
				d += diff * diff
			}
			if d < bestD {
				bestD = d
				bestK = j
			}
		}
		assignments[i] = bestK
		ivf.Lists[bestK] = append(ivf.Lists[bestK], i)
	}

	ivf.trained = true
	return nil
}

func (ivf *IVF) Search(query []float32, nprobe, topK int) []int {
	type centroidDist struct {
		idx  int
		dist float32
	}

	dists := make([]centroidDist, len(ivf.Centroids))
	for j, c := range ivf.Centroids {
		d := float32(0)
		for di := 0; di < ivf.Dim; di++ {
			diff := query[di] - c[di]
			d += diff * diff
		}
		dists[j] = centroidDist{idx: j, dist: d}
	}

	sort.Slice(dists, func(i, j int) bool {
		return dists[i].dist < dists[j].dist
	})

	if nprobe > len(dists) {
		nprobe = len(dists)
	}

	seen := make(map[int]bool)
	var results []int
	for _, cd := range dists[:nprobe] {
		for _, idx := range ivf.Lists[cd.idx] {
			if !seen[idx] {
				seen[idx] = true
				results = append(results, idx)
			}
		}
	}

	if len(results) > topK {
		results = results[:topK]
	}
	return results
}

func (ivf *IVF) SearchScored(query []float32, nprobe, topK int, distFunc func(a, b []float32) float32) []struct {
	Idx  int
	Dist float32
} {
	type centroidDist struct {
		idx  int
		dist float32
	}

	dists := make([]centroidDist, len(ivf.Centroids))
	for j, c := range ivf.Centroids {
		dd := float32(0)
		for di := 0; di < ivf.Dim; di++ {
			diff := query[di] - c[di]
			dd += diff * diff
		}
		dists[j] = centroidDist{idx: j, dist: dd}
	}

	sort.Slice(dists, func(i, j int) bool {
		return dists[i].dist < dists[j].dist
	})

	if nprobe > len(dists) {
		nprobe = len(dists)
	}

	type scoredResult struct {
		Idx  int
		Dist float32
	}
	var candidates []scoredResult

	for _, cd := range dists[:nprobe] {
		for _, idx := range ivf.Lists[cd.idx] {
			_ = distFunc
			candidates = append(candidates, scoredResult{Idx: idx, Dist: 0})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Dist < candidates[j].Dist
	})

	if len(candidates) > topK {
		candidates = candidates[:topK]
	}

	out := make([]struct {
		Idx  int
		Dist float32
	}, len(candidates))
	for i, c := range candidates {
		out[i] = struct {
			Idx  int
			Dist float32
		}{Idx: c.Idx, Dist: c.Dist}
	}
	return out
}
