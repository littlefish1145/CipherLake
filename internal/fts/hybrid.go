package fts

import (
	"context"
	"math"
	"sort"
	"strings"
)

// HybridWeights configures the weights for hybrid search fusion.
type HybridWeights struct {
	BM25Weight   float64 // default 0.5
	VectorWeight float64 // default 0.5
	RRFConstant  int     // default 60
}

// DefaultHybridWeights returns the default hybrid search weights.
func DefaultHybridWeights() HybridWeights {
	return HybridWeights{
		BM25Weight:   0.5,
		VectorWeight: 0.5,
		RRFConstant:  60,
	}
}

// HybridResult represents a result from hybrid search combining BM25 and vector scores.
type HybridResult struct {
	DocID  uint64
	Bucket string
	Key    string
	Score  float64
}

// VectorSearchResult represents a vector search result for integration.
type VectorSearchResult struct {
	Bucket    string
	ObjectKey string
	Score     float32
}

// HybridSearcher combines FTS (BM25) and vector search using RRF fusion.
type HybridSearcher struct {
	ftsIndex *InvertedIndex
}

// NewHybridSearcher creates a new hybrid searcher.
func NewHybridSearcher(ftsIndex *InvertedIndex) *HybridSearcher {
	return &HybridSearcher{
		ftsIndex: ftsIndex,
	}
}

// Search performs hybrid search combining BM25 and vector results using RRF fusion.
// vectorResults are provided externally from the vector search engine.
func (hs *HybridSearcher) Search(ctx context.Context, query string, topK int, weights HybridWeights, vectorResults []VectorSearchResult) ([]HybridResult, error) {
	if weights.BM25Weight == 0 && weights.VectorWeight == 0 {
		weights = DefaultHybridWeights()
	}
	if weights.RRFConstant <= 0 {
		weights.RRFConstant = 60
	}

	// Get BM25 results
	bm25Results, err := hs.ftsIndex.Search(query, topK*2) // get more for better fusion
	if err != nil {
		return nil, err
	}

	// Compute RRF scores
	rrfScores := make(map[uint64]float64)

	// BM25 ranking
	sort.Slice(bm25Results, func(i, j int) bool {
		return bm25Results[i].Score > bm25Results[j].Score
	})
	for rank, result := range bm25Results {
		rrfScores[result.DocID] += weights.BM25Weight / float64(weights.RRFConstant+rank+1)
	}

	// Vector ranking - map vector results to docIDs
	vectorRankMap := make(map[string]int) // bucket/key -> rank
	for rank, vr := range vectorResults {
		mapKey := vr.Bucket + "/" + vr.ObjectKey
		vectorRankMap[mapKey] = rank
	}

	// Match vector results to FTS docIDs
	hs.ftsIndex.mu.RLock()
	for docID, info := range hs.ftsIndex.docs {
		mapKey := info.Bucket + "/" + info.Key
		if rank, ok := vectorRankMap[mapKey]; ok {
			rrfScores[docID] += weights.VectorWeight / float64(weights.RRFConstant+rank+1)
		}
	}
	hs.ftsIndex.mu.RUnlock()

	// Also add vector-only results (not in FTS index)
	vectorOnlyResults := make(map[string]VectorSearchResult)
	for _, vr := range vectorResults {
		mapKey := vr.Bucket + "/" + vr.ObjectKey
		vectorOnlyResults[mapKey] = vr
	}

	// Remove results that are already in FTS (already counted above)
	for _, result := range bm25Results {
		if info, ok := hs.ftsIndex.GetDocInfo(result.DocID); ok {
			mapKey := info.Bucket + "/" + info.Key
			delete(vectorOnlyResults, mapKey)
		}
	}

	// Add vector-only results with a synthetic docID
	for mapKey, vr := range vectorOnlyResults {
		if rank, ok := vectorRankMap[mapKey]; ok {
			syntheticID := ComputeDocID(vr.Bucket, vr.ObjectKey, "vector")
			rrfScores[syntheticID] = weights.VectorWeight / float64(weights.RRFConstant+rank+1)
		}
	}

	// Sort by RRF score
	type scoredResult struct {
		docID uint64
		score float64
	}
	var results []scoredResult
	for docID, score := range rrfScores {
		results = append(results, scoredResult{docID, score})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if len(results) > topK {
		results = results[:topK]
	}

	// Build hybrid results
	hybridResults := make([]HybridResult, len(results))
	for i, r := range results {
		hr := HybridResult{
			DocID: r.docID,
			Score: r.score,
		}
		if info, ok := hs.ftsIndex.GetDocInfo(r.docID); ok {
			hr.Bucket = info.Bucket
			hr.Key = info.Key
		} else {
			// Check vector-only results
			for _, vr := range vectorResults {
				syntheticID := ComputeDocID(vr.Bucket, vr.ObjectKey, "vector")
				if syntheticID == r.docID {
					hr.Bucket = vr.Bucket
					hr.Key = vr.ObjectKey
					break
				}
			}
		}
		hybridResults[i] = hr
	}

	return hybridResults, nil
}

// GenerateSnippet creates a text snippet with highlighted terms around the first match.
func GenerateSnippet(text string, queryTokens []Token, snippetLen int) string {
	if snippetLen <= 0 {
		snippetLen = 100
	}

	lowerText := strings.ToLower(text)
	bestPos := -1

	// Find the position of the first matching term
	for _, qt := range queryTokens {
		idx := strings.Index(lowerText, qt.Term)
		if idx >= 0 && (bestPos == -1 || idx < bestPos) {
			bestPos = idx
		}
	}

	if bestPos == -1 {
		// No match found, return beginning of text
		if len(text) > snippetLen {
			return text[:snippetLen] + "..."
		}
		return text
	}

	// Calculate snippet window centered on the match
	halfLen := snippetLen / 2
	start := bestPos - halfLen
	if start < 0 {
		start = 0
	}
	end := start + snippetLen
	if end > len(text) {
		end = len(text)
	}

	snippet := text[start:end]

	// Add ellipsis if truncated
	prefix := ""
	suffix := ""
	if start > 0 {
		prefix = "..."
	}
	if end < len(text) {
		suffix = "..."
	}

	return prefix + snippet + suffix
}

// HighlightTerms wraps matching terms in <em> tags for highlighting.
func HighlightTerms(text string, queryTokens []Token) string {
	result := text
	for _, qt := range queryTokens {
		// Case-insensitive replacement
		lowerResult := strings.ToLower(result)
		var builder strings.Builder
		lastIdx := 0

		for {
			idx := strings.Index(lowerResult[lastIdx:], qt.Term)
			if idx == -1 {
				builder.WriteString(result[lastIdx:])
				break
			}
			absIdx := lastIdx + idx
			builder.WriteString(result[lastIdx:absIdx])
			builder.WriteString("<em>")
			builder.WriteString(result[absIdx : absIdx+len(qt.Term)])
			builder.WriteString("</em>")
			lastIdx = absIdx + len(qt.Term)
		}
		result = builder.String()
	}
	return result
}

// rrfScore computes the Reciprocal Rank Fusion score.
func rrfScore(rank int, k int) float64 {
	return 1.0 / float64(k+rank)
}

// ComputeRRFScore is exported for testing and external use.
func ComputeRRFScore(rank int, k int) float64 {
	return rrfScore(rank, k)
}

// Ensure math import is used
var _ = math.Pi
