package simd

const MaxTopK = 256

type TopKSelector struct {
	Scores [MaxTopK]float32
	Idxs   [MaxTopK]int32
	k      int
	filled int
}

func (s *TopKSelector) Init(k int) {
	if k > MaxTopK {
		k = MaxTopK
	}
	s.k = k
	s.filled = 0
}

func (s *TopKSelector) Push(score float32, idx int32) {
	if s.filled < s.k {
		s.Scores[s.filled] = score
		s.Idxs[s.filled] = idx
		s.filled++
		if s.filled == s.k {
			s.siftDown()
		}
		return
	}

	if score >= s.Scores[0] {
		return
	}

	s.Scores[0] = score
	s.Idxs[0] = idx
	s.siftDown()
}

func (s *TopKSelector) PushBatch(scores []float32, baseIdx int32) {
	for i, sc := range scores {
		s.Push(sc, baseIdx+int32(i))
	}
}

func (s *TopKSelector) siftDown() {
	i := 0
	n := s.k
	for {
		left := 2*i + 1
		right := 2*i + 2
		worst := i

		if left < n && s.Scores[left] > s.Scores[worst] {
			worst = left
		}
		if right < n && s.Scores[right] > s.Scores[worst] {
			worst = right
		}
		if worst == i {
			break
		}
		s.Scores[i], s.Scores[worst] = s.Scores[worst], s.Scores[i]
		s.Idxs[i], s.Idxs[worst] = s.Idxs[worst], s.Idxs[i]
		i = worst
	}
}

func (s *TopKSelector) Result() ([]float32, []int32) {
	s.sortResults()
	return s.Scores[:s.filled], s.Idxs[:s.filled]
}

func (s *TopKSelector) WriteResults(scores []float32, idxs []int32) int {
	s.sortResults()
	n := s.filled
	for i := 0; i < n && i < len(scores); i++ {
		scores[i] = s.Scores[i]
		idxs[i] = s.Idxs[i]
	}
	if n > len(scores) {
		n = len(scores)
	}
	return n
}

func (s *TopKSelector) Count() int {
	return s.filled
}

func (s *TopKSelector) sortResults() {
	for i := 0; i < s.filled; i++ {
		best := i
		for j := i + 1; j < s.filled; j++ {
			if s.Scores[j] < s.Scores[best] {
				best = j
			}
		}
		if best != i {
			s.Scores[i], s.Scores[best] = s.Scores[best], s.Scores[i]
			s.Idxs[i], s.Idxs[best] = s.Idxs[best], s.Idxs[i]
		}
	}
}

func TopK(dists []float32, k int) ([]float32, []int32) {
	n := len(dists)
	if n == 0 || k <= 0 {
		return nil, nil
	}
	if k > n {
		k = n
	}

	var sel TopKSelector
	sel.Init(k)
	for i, d := range dists {
		sel.Push(d, int32(i))
	}
	return sel.Result()
}
