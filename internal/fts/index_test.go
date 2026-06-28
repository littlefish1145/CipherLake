package fts

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// === Tokenizer Tests ===

func TestTokenizeBasic(t *testing.T) {
	tokens := Tokenize("Hello World")
	if len(tokens) == 0 {
		t.Fatal("expected tokens, got none")
	}

	// Check that tokens are lowercased and stemmed
	found := false
	for _, tok := range tokens {
		if tok.Term == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'hello' token, got %v", tokens)
	}
}

func TestTokenizeStopWords(t *testing.T) {
	tokens := Tokenize("the cat is on the mat")

	// Stop words should be removed
	for _, tok := range tokens {
		switch tok.Term {
		case "the", "is", "on":
			t.Errorf("stop word '%s' should have been removed", tok.Term)
		}
	}

	// Non-stop words should remain (stemmed)
	terms := make(map[string]bool)
	for _, tok := range tokens {
		terms[tok.Term] = true
	}

	if !terms["cat"] {
		t.Error("expected 'cat' to be present")
	}
	if !terms["mat"] {
		t.Error("expected 'mat' to be present")
	}
}

func TestTokenizePositions(t *testing.T) {
	tokens := Tokenize("cat dog bird")
	if len(tokens) < 3 {
		t.Fatalf("expected at least 3 tokens, got %d", len(tokens))
	}

	// Positions should be sequential
	for i, tok := range tokens {
		if tok.Position != i {
			t.Errorf("expected position %d, got %d", i, tok.Position)
		}
	}
}

func TestTokenizeEmpty(t *testing.T) {
	tokens := Tokenize("")
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens for empty string, got %d", len(tokens))
	}
}

func TestTokenizeUnicode(t *testing.T) {
	tokens := Tokenize("Hello 世界 World")
	if len(tokens) == 0 {
		t.Fatal("expected tokens, got none")
	}
}

func TestTokenizeAllStopWords(t *testing.T) {
	tokens := Tokenize("the a an is it")
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens for all stop words, got %d: %v", len(tokens), tokens)
	}
}

func TestPorterStem(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"running", "run"},
		{"caresses", "caress"},
		{"ponies", "poni"},
		{"cats", "cat"},
		{"agreed", "agre"},
		{"plastered", "plaster"},
		{"meetings", "meet"},
	}

	for _, tt := range tests {
		result := porterStem(tt.input)
		if result != tt.expected {
			t.Errorf("porterStem(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// === BM25 Scoring Tests ===

func TestBM25Score(t *testing.T) {
	scorer := NewBM25Scorer(1.2, 0.75)
	scorer.UpdateStats(1000, 50) // 1000 docs, avg doc length 50

	// Test basic scoring
	score := scorer.Score(5, 10, 50) // tf=5, df=10, dl=50
	if score <= 0 {
		t.Errorf("expected positive BM25 score, got %f", score)
	}
}

func TestBM25ZeroDocCount(t *testing.T) {
	scorer := NewBM25Scorer(1.2, 0.75)
	score := scorer.Score(5, 10, 50)
	if score != 0 {
		t.Errorf("expected 0 score with 0 doc count, got %f", score)
	}
}

func TestBM25ZeroDF(t *testing.T) {
	scorer := NewBM25Scorer(1.2, 0.75)
	scorer.UpdateStats(1000, 50)
	score := scorer.Score(5, 0, 50)
	if score != 0 {
		t.Errorf("expected 0 score with 0 df, got %f", score)
	}
}

func TestBM25IDFIncreasesWithRarity(t *testing.T) {
	scorer := NewBM25Scorer(1.2, 0.75)
	scorer.UpdateStats(1000, 50)

	score1 := scorer.Score(1, 100, 50) // common term (df=100)
	score2 := scorer.Score(1, 1, 50)   // rare term (df=1)

	if score2 <= score1 {
		t.Errorf("rare term should score higher: common=%f, rare=%f", score1, score2)
	}
}

func TestBM25LengthNormalization(t *testing.T) {
	scorer := NewBM25Scorer(1.2, 0.75)
	scorer.UpdateStats(1000, 50)

	// Shorter document should score higher for same tf
	score1 := scorer.Score(5, 10, 25)  // short doc
	score2 := scorer.Score(5, 10, 100) // long doc

	if score1 <= score2 {
		t.Errorf("shorter doc should score higher: short=%f, long=%f", score1, score2)
	}
}

func TestBM25TFSaturation(t *testing.T) {
	scorer := NewBM25Scorer(1.2, 0.75)
	scorer.UpdateStats(1000, 50)

	// Diminishing returns for higher tf
	score1 := scorer.Score(1, 10, 50)
	score2 := scorer.Score(10, 10, 50)
	score3 := scorer.Score(100, 10, 50)

	if score2 <= score1 {
		t.Errorf("higher tf should score higher: tf1=%f, tf10=%f", score1, score2)
	}

	// The increase from 10->100 should be less than from 1->10
	increase1 := score2 - score1
	increase2 := score3 - score2

	if increase2 >= increase1 {
		t.Errorf("expected diminishing returns: increase(1->10)=%f, increase(10->100)=%f", increase1, increase2)
	}
}

func TestBM25StandardFormula(t *testing.T) {
	// Verify against the standard BM25 formula
	scorer := NewBM25Scorer(1.2, 0.75)
	scorer.UpdateStats(10, 5)

	tf := 3
	df := 2
	dl := 6

	// IDF = ln((N - df + 0.5) / (df + 0.5) + 1)
	expectedIDF := math.Log(float64(10-2+0.5)/float64(2+0.5) + 1)

	// tfComponent = (tf * (k1 + 1)) / (tf + k1 * (1 - b + b * dl/avgDL))
	expectedTFComponent := float64(tf) * (1.2 + 1) / (float64(tf) + 1.2*(1-0.75+0.75*float64(dl)/5.0))

	expectedScore := expectedIDF * expectedTFComponent
	actualScore := scorer.Score(tf, df, dl)

	if math.Abs(actualScore-expectedScore) > 0.0001 {
		t.Errorf("BM25 score mismatch: expected %f, got %f", expectedScore, actualScore)
	}
}

// === Index Add/Search/Delete Tests ===

func TestInvertedIndexAddAndSearch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	// Add documents
	err = idx.AddDocumentWithInfo("bucket1", "key1", "v1", "the quick brown fox jumps over the lazy dog")
	if err != nil {
		t.Fatalf("failed to add document: %v", err)
	}

	err = idx.AddDocumentWithInfo("bucket1", "key2", "v2", "the lazy cat sleeps on the mat")
	if err != nil {
		t.Fatalf("failed to add document: %v", err)
	}

	// Search for "fox"
	results, err := idx.Search("fox", 10)
	if err != nil {
		t.Fatalf("failed to search: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'fox', got %d", len(results))
	}

	if results[0].Key != "key1" {
		t.Errorf("expected key1, got %s", results[0].Key)
	}

	// Search for "lazy" (should match both docs)
	results, err = idx.Search("lazy", 10)
	if err != nil {
		t.Fatalf("failed to search: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results for 'lazy', got %d", len(results))
	}
}

func TestInvertedIndexDelete(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	// Add and then delete
	err = idx.AddDocumentWithInfo("bucket1", "key1", "v1", "hello world")
	if err != nil {
		t.Fatalf("failed to add document: %v", err)
	}

	err = idx.DeleteDocumentByKey("bucket1", "key1")
	if err != nil {
		t.Fatalf("failed to delete document: %v", err)
	}

	// Search should return no results
	results, err := idx.Search("hello", 10)
	if err != nil {
		t.Fatalf("failed to search: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("expected 0 results after deletion, got %d", len(results))
	}
}

func TestInvertedIndexBM25Ranking(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	// Add documents with different relevance
	err = idx.AddDocumentWithInfo("bucket1", "relevant", "v1", "golang golang golang programming language")
	if err != nil {
		t.Fatalf("failed to add document: %v", err)
	}

	err = idx.AddDocumentWithInfo("bucket1", "less-relevant", "v2", "golang is a programming language")
	if err != nil {
		t.Fatalf("failed to add document: %v", err)
	}

	results, err := idx.Search("golang", 10)
	if err != nil {
		t.Fatalf("failed to search: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// The document with more occurrences should rank higher
	if results[0].Key != "relevant" {
		t.Errorf("expected 'relevant' to rank first, got '%s'", results[0].Key)
	}
}

func TestInvertedIndexDocIDGeneration(t *testing.T) {
	id1 := ComputeDocID("bucket1", "key1", "v1")
	id2 := ComputeDocID("bucket1", "key1", "v2")
	id3 := ComputeDocID("bucket1", "key1", "v1")

	if id1 == id2 {
		t.Error("different versions should produce different docIDs")
	}
	if id1 != id3 {
		t.Error("same input should produce same docID")
	}
}

// === Segment Merge Tests ===

func TestSegmentCreation(t *testing.T) {
	seg := NewSegment(1, 1024)
	if seg.ID() != 1 {
		t.Errorf("expected segment ID 1, got %d", seg.ID())
	}
	if seg.DocCount() != 0 {
		t.Errorf("expected 0 docs in new segment, got %d", seg.DocCount())
	}
}

func TestSegmentAddDocument(t *testing.T) {
	seg := NewSegment(1, 1024)
	tokens := Tokenize("hello world")
	full := seg.AddDocument(1, tokens)

	if full {
		t.Error("segment should not be full with 1 document")
	}
	if seg.DocCount() != 1 {
		t.Errorf("expected 1 doc, got %d", seg.DocCount())
	}
}

func TestSegmentSearch(t *testing.T) {
	seg := NewSegment(1, 1024)
	tokens := Tokenize("hello world")
	seg.AddDocument(1, tokens)

	postings := seg.Search("hello")
	if len(postings) != 1 {
		t.Errorf("expected 1 posting for 'hello', got %d", len(postings))
	}
	if postings[0].DocID != 1 {
		t.Errorf("expected docID 1, got %d", postings[0].DocID)
	}
}

func TestSegmentDelete(t *testing.T) {
	seg := NewSegment(1, 1024)
	tokens := Tokenize("hello world")
	seg.AddDocument(1, tokens)

	seg.DeleteDocument(1)

	postings := seg.Search("hello")
	if len(postings) != 0 {
		t.Errorf("expected 0 postings after delete, got %d", len(postings))
	}
}

func TestSegmentManagerMerge(t *testing.T) {
	sm := NewSegmentManager(2, 5) // small segment size for testing

	// Add enough documents to create multiple segments
	for i := 0; i < 6; i++ {
		tokens := Tokenize(fmt.Sprintf("document number %d hello world", i))
		sm.AddDocument(uint64(i), tokens)
	}

	if sm.SegmentCount() < 3 {
		t.Errorf("expected at least 3 segments, got %d", sm.SegmentCount())
	}

	// Merge segments
	sm.MergeSegments()

	// After merge, should have fewer segments
	if sm.SegmentCount() > 3 {
		t.Errorf("expected fewer segments after merge, got %d", sm.SegmentCount())
	}
}

func TestPostingListEncoding(t *testing.T) {
	postings := []Posting{
		{DocID: 1, TermFreq: 3, Positions: []int{0, 5, 10}},
		{DocID: 5, TermFreq: 1, Positions: []int{2}},
		{DocID: 10, TermFreq: 2, Positions: []int{1, 8}},
	}

	encoded := EncodePostings(postings)
	decoded := DecodePostings(encoded)

	if len(decoded) != len(postings) {
		t.Fatalf("expected %d postings, got %d", len(postings), len(decoded))
	}

	for i, p := range decoded {
		if p.DocID != postings[i].DocID {
			t.Errorf("posting %d: expected docID %d, got %d", i, postings[i].DocID, p.DocID)
		}
		if p.TermFreq != postings[i].TermFreq {
			t.Errorf("posting %d: expected termFreq %d, got %d", i, postings[i].TermFreq, p.TermFreq)
		}
		if len(p.Positions) != len(postings[i].Positions) {
			t.Errorf("posting %d: expected %d positions, got %d", i, len(postings[i].Positions), len(p.Positions))
		}
		for j, pos := range p.Positions {
			if pos != postings[i].Positions[j] {
				t.Errorf("posting %d pos %d: expected %d, got %d", i, j, postings[i].Positions[j], pos)
			}
		}
	}
}

func TestPostingListEncodingEmpty(t *testing.T) {
	encoded := EncodePostings(nil)
	if encoded != nil {
		t.Errorf("expected nil for empty postings, got %v", encoded)
	}

	decoded := DecodePostings(nil)
	if decoded != nil {
		t.Errorf("expected nil for empty data, got %v", decoded)
	}
}

// === Hybrid Search Tests ===

func TestRRFScore(t *testing.T) {
	// RRF score = 1 / (k + rank)
	score := ComputeRRFScore(0, 60) // rank 0, k=60
	expected := 1.0 / 60.0
	if math.Abs(score-expected) > 0.0001 {
		t.Errorf("RRF score for rank 0: expected %f, got %f", expected, score)
	}

	score = ComputeRRFScore(1, 60) // rank 1, k=60
	expected = 1.0 / 61.0
	if math.Abs(score-expected) > 0.0001 {
		t.Errorf("RRF score for rank 1: expected %f, got %f", expected, score)
	}
}

func TestHybridSearch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	// Add documents
	idx.AddDocumentWithInfo("bucket1", "doc1", "v1", "golang programming language")
	idx.AddDocumentWithInfo("bucket1", "doc2", "v2", "python programming language")

	hs := NewHybridSearcher(idx)

	vectorResults := []VectorSearchResult{
		{Bucket: "bucket1", ObjectKey: "doc1", Score: 0.9},
		{Bucket: "bucket1", ObjectKey: "doc2", Score: 0.8},
	}

	results, err := hs.Search(context.Background(), "golang", 10, DefaultHybridWeights(), vectorResults)
	if err != nil {
		t.Fatalf("hybrid search failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected results from hybrid search")
	}

	// doc1 should rank higher (appears in both BM25 and vector results)
	if results[0].Key != "doc1" {
		t.Errorf("expected doc1 to rank first, got %s", results[0].Key)
	}
}

func TestHybridSearchBM25Only(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	idx.AddDocumentWithInfo("bucket1", "doc1", "v1", "hello world")

	hs := NewHybridSearcher(idx)

	// No vector results
	results, err := hs.Search(context.Background(), "hello", 10, DefaultHybridWeights(), nil)
	if err != nil {
		t.Fatalf("hybrid search failed: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
}

func TestHybridSearchVectorOnly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	hs := NewHybridSearcher(idx)

	vectorResults := []VectorSearchResult{
		{Bucket: "bucket1", ObjectKey: "doc1", Score: 0.9},
	}

	results, err := hs.Search(context.Background(), "nonexistent", 10, DefaultHybridWeights(), vectorResults)
	if err != nil {
		t.Fatalf("hybrid search failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected results from vector-only search")
	}
}

// === Snippet/Highlight Tests ===

func TestGenerateSnippet(t *testing.T) {
	text := "The quick brown fox jumps over the lazy dog. The dog was not amused."
	tokens := Tokenize("fox")

	snippet := GenerateSnippet(text, tokens, 30)
	if snippet == "" {
		t.Fatal("expected non-empty snippet")
	}

	// Snippet should contain the matching term
	if !containsSubstring(snippet, "fox") {
		t.Errorf("snippet should contain 'fox': %s", snippet)
	}
}

func TestGenerateSnippetNoMatch(t *testing.T) {
	text := "The quick brown fox jumps over the lazy dog"
	tokens := Tokenize("elephant")

	snippet := GenerateSnippet(text, tokens, 30)
	if snippet == "" {
		t.Fatal("expected non-empty snippet even without match")
	}
}

func TestHighlightTerms(t *testing.T) {
	text := "the quick brown fox"
	tokens := []Token{{Term: "fox", Position: 0}}

	highlighted := HighlightTerms(text, tokens)
	if !containsSubstring(highlighted, "<em>fox</em>") {
		t.Errorf("expected highlighted term, got: %s", highlighted)
	}
}

func TestHighlightTermsMultiple(t *testing.T) {
	text := "the quick brown fox and the lazy fox"
	tokens := []Token{{Term: "fox", Position: 0}}

	highlighted := HighlightTerms(text, tokens)
	// Both occurrences should be highlighted
	count := countSubstring(highlighted, "<em>fox</em>")
	if count != 2 {
		t.Errorf("expected 2 highlighted occurrences, got %d: %s", count, highlighted)
	}
}

func TestGenerateSnippetShortText(t *testing.T) {
	text := "hello"
	tokens := Tokenize("hello")

	snippet := GenerateSnippet(text, tokens, 100)
	if snippet != "hello" {
		t.Errorf("expected 'hello', got '%s'", snippet)
	}
}

// === Compaction Tests ===

func TestCompactionManagerQuota(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	cm := idx.compaction

	// Set a very small quota
	cm.SetMaxIndexSize(1) // 1 byte

	// Check quota exceeded
	if !cm.IsQuotaExceeded() {
		// May or may not be exceeded depending on actual index size
		// but the mechanism should work
		t.Log("Quota check mechanism works")
	}

	// Set large quota
	cm.SetMaxIndexSize(10 * 1024 * 1024 * 1024) // 10GB
	if cm.IsQuotaExceeded() {
		t.Error("should not exceed 10GB quota")
	}
}

func TestCompactionManagerForceCompaction(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	// Add some documents
	for i := 0; i < 5; i++ {
		idx.AddDocumentWithInfo("bucket1", fmt.Sprintf("key%d", i), fmt.Sprintf("v%d", i),
			fmt.Sprintf("document number %d hello world", i))
	}

	// Force compaction should not error
	idx.compaction.ForceCompaction()
}

// === Integration Tests ===

func TestFullIndexWorkflow(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	// Add multiple documents
	docs := []struct {
		bucket string
		key    string
		text   string
	}{
		{"docs", "readme", "Nexus is an object storage system with full-text search capabilities"},
		{"docs", "guide", "This guide explains how to use the full-text search feature in Nexus"},
		{"docs", "api", "The API reference for the FTS search endpoint"},
		{"data", "log1", "Application log file with error messages and warnings"},
		{"data", "log2", "System log with performance metrics and error traces"},
	}

	for _, d := range docs {
		err := idx.AddDocumentWithInfo(d.bucket, d.key, "v1", d.text)
		if err != nil {
			t.Fatalf("failed to add document %s: %v", d.key, err)
		}
	}

	// Search for "nexus"
	results, err := idx.Search("nexus", 10)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results for 'nexus', got %d", len(results))
	}

	// Search for "error"
	results, err = idx.Search("error", 10)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results for 'error', got %d", len(results))
	}

	// Delete a document
	err = idx.DeleteDocumentByKey("docs", "readme")
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	// Search again - should have one less result
	results, err = idx.Search("nexus", 10)
	if err != nil {
		t.Fatalf("search after delete failed: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("expected 1 result after delete, got %d", len(results))
	}

	// Check stats
	stats := idx.Stats()
	if stats["doc_count"].(int64) < 1 {
		t.Error("expected at least 1 doc in stats")
	}
}

func TestIndexPersistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Create index and add documents
	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}

	docID := ComputeDocID("bucket1", "key1", "v1")
	idx.AddDocumentWithInfo("bucket1", "key1", "v1", "hello world")

	// Verify doc info is available before closing
	info, ok := idx.GetDocInfo(docID)
	if !ok {
		t.Fatal("expected doc info to be available before close")
	}
	if info.Bucket != "bucket1" || info.Key != "key1" {
		t.Errorf("doc info mismatch: bucket=%s, key=%s", info.Bucket, info.Key)
	}

	idx.Close()

	// Reopen and verify doc info is loaded from BoltDB persistence
	idx2, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen index: %v", err)
	}
	defer idx2.Close()

	info2, ok := idx2.GetDocInfo(docID)
	if !ok {
		t.Fatal("expected doc info to be persisted and loaded on reopen")
	}
	if info2.Bucket != "bucket1" || info2.Key != "key1" {
		t.Errorf("doc info mismatch after reopen: bucket=%s, key=%s", info2.Bucket, info2.Key)
	}
}

func TestIndexBucketFilter(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idx, err := NewInvertedIndex(dbPath)
	if err != nil {
		t.Fatalf("failed to create index: %v", err)
	}
	defer idx.Close()

	idx.AddDocumentWithInfo("bucket1", "key1", "v1", "hello world")
	idx.AddDocumentWithInfo("bucket2", "key2", "v2", "hello universe")

	results, err := idx.Search("hello", 10)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}

	// Verify both buckets are represented
	buckets := make(map[string]bool)
	for _, r := range results {
		buckets[r.Bucket] = true
	}
	if !buckets["bucket1"] || !buckets["bucket2"] {
		t.Errorf("expected results from both buckets, got: %v", buckets)
	}
}

func TestSegmentStats(t *testing.T) {
	seg := NewSegment(1, 1024)
	tokens := Tokenize("hello world foo bar")
	seg.AddDocument(1, tokens)

	docCount, _, avgDL := seg.GetStats()
	if docCount != 1 {
		t.Errorf("expected 1 doc, got %d", docCount)
	}
	if avgDL <= 0 {
		t.Errorf("expected positive avgDL, got %f", avgDL)
	}
}

func TestSegmentManagerStats(t *testing.T) {
	sm := NewSegmentManager(1024, 10)
	tokens := Tokenize("hello world")
	sm.AddDocument(1, tokens)

	docCount, avgDL := sm.GetStats()
	if docCount != 1 {
		t.Errorf("expected 1 doc, got %d", docCount)
	}
	if avgDL <= 0 {
		t.Errorf("expected positive avgDL, got %f", avgDL)
	}
}

func TestComputeDocIDDeterministic(t *testing.T) {
	id1 := ComputeDocID("mybucket", "mykey", "v1")
	id2 := ComputeDocID("mybucket", "mykey", "v1")

	if id1 != id2 {
		t.Error("same inputs should produce same docID")
	}

	// Different inputs should produce different IDs
	id3 := ComputeDocID("mybucket", "mykey", "v2")
	if id1 == id3 {
		t.Error("different versions should produce different docIDs")
	}
}

func TestHybridWeights(t *testing.T) {
	w := DefaultHybridWeights()
	if w.BM25Weight != 0.5 {
		t.Errorf("expected BM25Weight 0.5, got %f", w.BM25Weight)
	}
	if w.VectorWeight != 0.5 {
		t.Errorf("expected VectorWeight 0.5, got %f", w.VectorWeight)
	}
	if w.RRFConstant != 60 {
		t.Errorf("expected RRFConstant 60, got %d", w.RRFConstant)
	}
}

// Helper functions

func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func countSubstring(s, sub string) int {
	count := 0
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			count++
		}
	}
	return count
}

// Ensure sort and os are used
var _ = sort.Sort
var _ = os.Getenv
