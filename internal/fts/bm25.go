package fts

import "math"

// BM25Scorer implements the BM25 scoring algorithm.
type BM25Scorer struct {
	K1       float64 // term frequency saturation parameter, default 1.2
	B        float64 // length normalization parameter, default 0.75
	AvgDL    float64 // average document length
	DocCount int64   // total number of documents
}

// NewBM25Scorer creates a new BM25 scorer with the given parameters.
func NewBM25Scorer(k1, b float64) *BM25Scorer {
	return &BM25Scorer{
		K1: k1,
		B:  b,
	}
}

// Score computes the BM25 score for a term in a document.
// tf = term frequency in the document
// df = document frequency (number of documents containing the term)
// dl = document length (number of terms in the document)
func (s *BM25Scorer) Score(tf int, df int, dl int) float64 {
	if s.DocCount == 0 || df == 0 {
		return 0
	}

	// IDF = ln((N - df + 0.5) / (df + 0.5) + 1)
	idf := math.Log((float64(s.DocCount)-float64(df)+0.5)/(float64(df)+0.5) + 1)
	// BM25 term frequency component
	avgDL := s.AvgDL
	if avgDL == 0 {
		avgDL = 1
	}
	tfFloat := float64(tf)
	dlFloat := float64(dl)

	numerator := tfFloat * (s.K1 + 1)
	denominator := tfFloat + s.K1*(1-s.B+s.B*dlFloat/avgDL)

	return idf * numerator / denominator
}

// UpdateStats updates the scorer's statistics for BM25 calculation.
func (s *BM25Scorer) UpdateStats(docCount int64, avgDL float64) {
	s.DocCount = docCount
	s.AvgDL = avgDL
}
