package fts

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// SearchResult represents a single search result from the FTS index.
type SearchResult struct {
	DocID  uint64
	Score  float64
	Bucket string
	Key    string
}

// DocInfo stores metadata about an indexed document.
type DocInfo struct {
	DocID   uint64
	Bucket  string
	Key     string
	Version string
	Length  int // document length in tokens
}

// InvertedIndex manages the full-text search inverted index with LSM-style segments.
type InvertedIndex struct {
	db       *bolt.DB
	dbPath   string
	mu       sync.RWMutex
	segments *SegmentManager
	scorer   *BM25Scorer
	docs     map[uint64]*DocInfo // docID -> document info
	termDF   map[string]int      // term -> document frequency

	compaction *CompactionManager
	closed     bool
}

// NewInvertedIndex creates a new inverted index backed by BoltDB.
func NewInvertedIndex(dbPath string) (*InvertedIndex, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create FTS directory: %w", err)
	}

	db, err := bolt.Open(dbPath, 0666, &bolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open FTS bolt db: %w", err)
	}

	// Create buckets
	if err := db.Update(func(tx *bolt.Tx) error {
		buckets := []string{"fts_terms", "fts_docs", "fts_stats"}
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", name, err)
			}
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize FTS buckets: %w", err)
	}

	segManager := NewSegmentManager(DefaultSegmentSize, 10)
	scorer := NewBM25Scorer(1.2, 0.75)

	idx := &InvertedIndex{
		db:       db,
		dbPath:   dbPath,
		segments: segManager,
		scorer:   scorer,
		docs:     make(map[uint64]*DocInfo),
		termDF:   make(map[string]int),
	}

	// Load existing document info from BoltDB
	idx.loadDocsFromDB()

	// Start compaction manager
	idx.compaction = NewCompactionManager(idx, 10*1024*1024*1024) // 10GB default
	idx.compaction.Start()

	return idx, nil
}

// SetCompactionMaxIndexSize sets the maximum index size for compaction.
func (idx *InvertedIndex) SetCompactionMaxIndexSize(maxSize int64) {
	if idx.compaction != nil {
		idx.compaction.SetMaxIndexSize(maxSize)
	}
}

// loadDocsFromDB loads document info from BoltDB on startup.
func (idx *InvertedIndex) loadDocsFromDB() error {
	return idx.db.View(func(tx *bolt.Tx) error {
		docsBucket := tx.Bucket([]byte("fts_docs"))
		if docsBucket == nil {
			return nil
		}

		cursor := docsBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			if len(v) < 14 { // minimum doc info size: 2+0+2+0+2+0+4=10, but realistically 14
				continue
			}
			docID := binary.BigEndian.Uint64(k[:8])
			bucketLen := int(binary.BigEndian.Uint16(v[0:2]))
			if len(v) < 2+bucketLen {
				continue
			}
			bucket := string(v[2 : 2+bucketLen])
			keyStart := 2 + bucketLen
			if len(v) < keyStart+2 {
				continue
			}
			keyLen := int(binary.BigEndian.Uint16(v[keyStart : keyStart+2]))
			if len(v) < keyStart+2+keyLen {
				continue
			}
			key := string(v[keyStart+2 : keyStart+2+keyLen])
			verStart := keyStart + 2 + keyLen
			if len(v) < verStart+2 {
				continue
			}
			verLen := int(binary.BigEndian.Uint16(v[verStart : verStart+2]))
			if len(v) < verStart+2+verLen+4 {
				continue
			}
			version := string(v[verStart+2 : verStart+2+verLen])
			docLen := int(binary.BigEndian.Uint32(v[verStart+2+verLen : verStart+2+verLen+4]))

			idx.docs[docID] = &DocInfo{
				DocID:   docID,
				Bucket:  bucket,
				Key:     key,
				Version: version,
				Length:  docLen,
			}
		}
		return nil
	})
}

// ComputeDocID generates a document ID from bucket, key, and versionID using SHA-256.
func ComputeDocID(bucket, key, versionID string) uint64 {
	h := sha256.New()
	h.Write([]byte(bucket + "#" + key + "#" + versionID))
	hash := h.Sum(nil)
	return binary.BigEndian.Uint64(hash[:8])
}

// AddDocument tokenizes text and adds it to the FTS index.
func (idx *InvertedIndex) AddDocument(docID uint64, tokens []Token) error {
	if idx.closed {
		return fmt.Errorf("index is closed")
	}

	// Check disk quota
	if idx.compaction != nil && idx.compaction.IsQuotaExceeded() {
		return fmt.Errorf("FTS index disk quota exceeded")
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Add to segment manager
	idx.segments.AddDocument(docID, tokens)

	// Update term document frequencies
	seen := make(map[string]bool)
	for _, t := range tokens {
		if !seen[t.Term] {
			idx.termDF[t.Term]++
			seen[t.Term] = true
		}
	}

	// Update scorer stats
	docCount, avgDL := idx.segments.GetStats()
	idx.scorer.UpdateStats(docCount, avgDL)

	return nil
}

// AddDocumentWithInfo adds a document with its metadata info.
func (idx *InvertedIndex) AddDocumentWithInfo(bucket, key, versionID, text string) error {
	docID := ComputeDocID(bucket, key, versionID)
	tokens := Tokenize(text)

	// Store doc info
	idx.mu.Lock()
	idx.docs[docID] = &DocInfo{
		DocID:   docID,
		Bucket:  bucket,
		Key:     key,
		Version: versionID,
		Length:  len(tokens),
	}
	idx.mu.Unlock()

	// Persist doc info to BoltDB
	if err := idx.persistDocInfo(docID, bucket, key, versionID, len(tokens)); err != nil {
		return fmt.Errorf("failed to persist doc info: %w", err)
	}

	return idx.AddDocument(docID, tokens)
}

// persistDocInfo stores document metadata in BoltDB.
func (idx *InvertedIndex) persistDocInfo(docID uint64, bucket, key, versionID string, docLen int) error {
	keyBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(keyBytes, docID)

	// Value format: [bucketLen(2)][bucket][keyLen(2)][key][verLen(2)][version][docLen(4)]
	val := make([]byte, 0, 2+len(bucket)+2+len(key)+2+len(versionID)+4)
	val = binary.BigEndian.AppendUint16(val, uint16(len(bucket)))
	val = append(val, bucket...)
	val = binary.BigEndian.AppendUint16(val, uint16(len(key)))
	val = append(val, key...)
	val = binary.BigEndian.AppendUint16(val, uint16(len(versionID)))
	val = append(val, versionID...)
	val = binary.BigEndian.AppendUint32(val, uint32(docLen))

	return idx.db.Update(func(tx *bolt.Tx) error {
		docsBucket := tx.Bucket([]byte("fts_docs"))
		return docsBucket.Put(keyBytes, val)
	})
}

// Search performs a BM25 search for the given query string.
func (idx *InvertedIndex) Search(query string, topK int) ([]SearchResult, error) {
	if idx.closed {
		return nil, fmt.Errorf("index is closed")
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	queryTokens := Tokenize(query)
	if len(queryTokens) == 0 {
		return nil, nil
	}

	// Collect scores for each document
	docScores := make(map[uint64]float64)
	seen := make(map[string]bool)

	for _, qt := range queryTokens {
		if seen[qt.Term] {
			continue
		}
		seen[qt.Term] = true

		postings := idx.segments.Search(qt.Term)
		df := idx.termDF[qt.Term]

		for _, p := range postings {
			dl := 0
			if info, ok := idx.docs[p.DocID]; ok {
				dl = info.Length
			}

			score := idx.scorer.Score(p.TermFreq, df, dl)
			docScores[p.DocID] += score
		}
	}

	// Sort by score descending
	type scoredDoc struct {
		docID uint64
		score float64
	}
	var results []scoredDoc
	for docID, score := range docScores {
		results = append(results, scoredDoc{docID, score})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	// Limit to topK
	if len(results) > topK {
		results = results[:topK]
	}

	// Build search results with doc info
	searchResults := make([]SearchResult, len(results))
	for i, r := range results {
		sr := SearchResult{
			DocID: r.docID,
			Score: r.score,
		}
		if info, ok := idx.docs[r.docID]; ok {
			sr.Bucket = info.Bucket
			sr.Key = info.Key
		}
		searchResults[i] = sr
	}

	return searchResults, nil
}

// DeleteDocument removes a document from the FTS index.
func (idx *InvertedIndex) DeleteDocument(docID uint64) error {
	if idx.closed {
		return fmt.Errorf("index is closed")
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Remove from segments
	idx.segments.DeleteDocument(docID)

	// Remove doc info
	delete(idx.docs, docID)

	// Remove from BoltDB
	keyBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(keyBytes, docID)
	if err := idx.db.Update(func(tx *bolt.Tx) error {
		docsBucket := tx.Bucket([]byte("fts_docs"))
		return docsBucket.Delete(keyBytes)
	}); err != nil {
		return fmt.Errorf("failed to delete doc info: %w", err)
	}

	// Update scorer stats
	docCount, avgDL := idx.segments.GetStats()
	idx.scorer.UpdateStats(docCount, avgDL)

	return nil
}

// DeleteDocumentByKey removes a document by bucket and key.
func (idx *InvertedIndex) DeleteDocumentByKey(bucket, key string) error {
	idx.mu.RLock()
	var targetDocID uint64
	found := false
	for docID, info := range idx.docs {
		if info.Bucket == bucket && info.Key == key {
			targetDocID = docID
			found = true
			break
		}
	}
	idx.mu.RUnlock()

	if !found {
		return nil
	}
	return idx.DeleteDocument(targetDocID)
}

// GetDocInfo returns document info for a given docID.
func (idx *InvertedIndex) GetDocInfo(docID uint64) (*DocInfo, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	info, ok := idx.docs[docID]
	if !ok {
		return nil, false
	}
	return info, true
}

// GetScorer returns the BM25 scorer for external use.
func (idx *InvertedIndex) GetScorer() *BM25Scorer {
	return idx.scorer
}

// GetSegmentManager returns the segment manager.
func (idx *InvertedIndex) GetSegmentManager() *SegmentManager {
	return idx.segments
}

// GetDB returns the underlying BoltDB instance.
func (idx *InvertedIndex) GetDB() *bolt.DB {
	return idx.db
}

// Close closes the inverted index and releases resources.
func (idx *InvertedIndex) Close() error {
	if idx.closed {
		return nil
	}
	idx.closed = true

	if idx.compaction != nil {
		idx.compaction.Stop()
	}

	return idx.db.Close()
}

// Stats returns index statistics.
func (idx *InvertedIndex) Stats() map[string]interface{} {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	docCount, avgDL := idx.segments.GetStats()

	return map[string]interface{}{
		"doc_count":      docCount,
		"avg_doc_length": avgDL,
		"segment_count":  idx.segments.SegmentCount(),
		"unique_terms":   len(idx.termDF),
	}
}
