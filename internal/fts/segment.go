package fts

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
)

// DefaultSegmentSize is the number of documents per segment before flushing.
const DefaultSegmentSize = 1024

// Posting represents a single entry in a posting list: docID, term frequency, and positions.
type Posting struct {
	DocID    uint64
	TermFreq int
	Positions []int
}

// Segment represents an in-memory or flushed index segment.
type Segment struct {
	mu       sync.RWMutex
	id       int
	postings map[string][]Posting // term -> posting list
	docLens  map[uint64]int       // docID -> document length
	docCount int
	flushed  bool
	maxDocs  int
}

// NewSegment creates a new in-memory segment.
func NewSegment(id, maxDocs int) *Segment {
	return &Segment{
		id:       id,
		postings: make(map[string][]Posting),
		docLens:  make(map[uint64]int),
		maxDocs:  maxDocs,
	}
}

// AddDocument adds a document's tokens to the segment.
// Returns true if the segment is full and should be flushed.
func (s *Segment) AddDocument(docID uint64, tokens []Token) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build term -> positions map for this document
	termPositions := make(map[string][]int)
	termFreq := make(map[string]int)
	for _, t := range tokens {
		termPositions[t.Term] = append(termPositions[t.Term], t.Position)
		termFreq[t.Term]++
	}

	// Add postings
	for term, positions := range termPositions {
		p := Posting{
			DocID:     docID,
			TermFreq:  termFreq[term],
			Positions: positions,
		}
		s.postings[term] = append(s.postings[term], p)
	}

	// Record document length
	s.docLens[docID] = len(tokens)
	s.docCount++

	return s.docCount >= s.maxDocs
}

// Search searches for a term in this segment and returns matching postings.
func (s *Segment) Search(term string) []Posting {
	s.mu.RLock()
	defer s.mu.RUnlock()

	postings, ok := s.postings[term]
	if !ok {
		return nil
	}

	// Return a copy to avoid data races
	result := make([]Posting, len(postings))
	copy(result, postings)
	return result
}

// DeleteDocument marks a document as deleted by removing its postings.
func (s *Segment) DeleteDocument(docID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove from docLens
	delete(s.docLens, docID)

	// Remove from all posting lists
	for term, postings := range s.postings {
		filtered := make([]Posting, 0, len(postings))
		for _, p := range postings {
			if p.DocID != docID {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			delete(s.postings, term)
		} else {
			s.postings[term] = filtered
		}
	}

	s.docCount--
}

// GetStats returns segment statistics.
func (s *Segment) GetStats() (docCount int, totalTerms int, avgDL float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalLen := 0
	for _, dl := range s.docLens {
		totalLen += dl
	}

	if s.docCount > 0 {
		avgDL = float64(totalLen) / float64(s.docCount)
	}

	return s.docCount, len(s.postings), avgDL
}

// DocCount returns the number of documents in the segment.
func (s *Segment) DocCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.docCount
}

// IsFlushed returns whether the segment has been flushed to disk.
func (s *Segment) IsFlushed() bool {
	return s.flushed
}

// SetFlushed marks the segment as flushed.
func (s *Segment) SetFlushed(f bool) {
	s.flushed = f
}

// ID returns the segment ID.
func (s *Segment) ID() int {
	return s.id
}

// Postings returns all postings in the segment (used for merging).
func (s *Segment) Postings() map[string][]Posting {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string][]Posting, len(s.postings))
	for term, postings := range s.postings {
		result[term] = make([]Posting, len(postings))
		copy(result[term], postings)
	}
	return result
}

// DocLens returns the document length map.
func (s *Segment) DocLens() map[uint64]int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[uint64]int, len(s.docLens))
	for k, v := range s.docLens {
		result[k] = v
	}
	return result
}

// SegmentManager handles segment lifecycle and background merging.
type SegmentManager struct {
	mu          sync.RWMutex
	segments    []*Segment
	mergeQueue  chan int
	maxSegments int
	nextSegID   int64
	activeSeg   *Segment
	segmentSize int
}

// NewSegmentManager creates a new segment manager.
func NewSegmentManager(segmentSize, maxSegments int) *SegmentManager {
	if segmentSize <= 0 {
		segmentSize = DefaultSegmentSize
	}
	if maxSegments <= 0 {
		maxSegments = 10
	}

	sm := &SegmentManager{
		segments:    make([]*Segment, 0),
		mergeQueue:  make(chan int, 100),
		maxSegments: maxSegments,
		segmentSize: segmentSize,
	}

	// Create initial active segment
	sm.activeSeg = NewSegment(int(atomic.AddInt64(&sm.nextSegID, 1)), segmentSize)
	sm.segments = append(sm.segments, sm.activeSeg)

	return sm
}

// AddDocument adds a document to the active segment.
// If the active segment is full, it creates a new one and signals for merge.
func (sm *SegmentManager) AddDocument(docID uint64, tokens []Token) {
	sm.mu.Lock()

	full := sm.activeSeg.AddDocument(docID, tokens)

	if full {
		// Flush current segment and create a new one
		sm.activeSeg.SetFlushed(true)
		newSeg := NewSegment(int(atomic.AddInt64(&sm.nextSegID, 1)), sm.segmentSize)
		sm.activeSeg = newSeg
		sm.segments = append(sm.segments, newSeg)

		sm.mu.Unlock()

		// Signal merge if too many segments
		if len(sm.segments) >= sm.maxSegments {
			select {
			case sm.mergeQueue <- len(sm.segments):
			default:
			}
		}
	} else {
		sm.mu.Unlock()
	}
}

// Search searches all segments for the given term.
func (sm *SegmentManager) Search(term string) []Posting {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var allPostings []Posting
	for _, seg := range sm.segments {
		postings := seg.Search(term)
		allPostings = append(allPostings, postings...)
	}
	return allPostings
}

// DeleteDocument removes a document from all segments.
func (sm *SegmentManager) DeleteDocument(docID uint64) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, seg := range sm.segments {
		seg.DeleteDocument(docID)
	}
}

// GetStats returns aggregate statistics across all segments.
func (sm *SegmentManager) GetStats() (docCount int64, avgDL float64) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	totalDocs := 0
	totalLen := 0.0

	for _, seg := range sm.segments {
		dc, _, adl := seg.GetStats()
		totalDocs += dc
		totalLen += adl * float64(dc)
	}

	if totalDocs > 0 {
		avgDL = totalLen / float64(totalDocs)
	}

	return int64(totalDocs), avgDL
}

// SegmentCount returns the number of segments.
func (sm *SegmentManager) SegmentCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.segments)
}

// MergeSegments merges multiple small segments into a single larger segment.
func (sm *SegmentManager) MergeSegments() {
	sm.mu.Lock()

	if len(sm.segments) <= 1 {
		sm.mu.Unlock()
		return
	}

	// Collect all segments except the active one
	var toMerge []*Segment
	var active *Segment
	for _, seg := range sm.segments {
		if seg == sm.activeSeg {
			active = seg
		} else {
			toMerge = append(toMerge, seg)
		}
	}

	if len(toMerge) <= 1 {
		sm.mu.Unlock()
		return
	}

	// Create merged segment
	merged := NewSegment(int(atomic.AddInt64(&sm.nextSegID, 1)), sm.segmentSize*len(toMerge))

	// Merge postings
	mergedPostings := make(map[string][]Posting)
	mergedDocLens := make(map[uint64]int)

	for _, seg := range toMerge {
		postings := seg.Postings()
		docLens := seg.DocLens()

		for term, plist := range postings {
			mergedPostings[term] = append(mergedPostings[term], plist...)
		}
		for docID, dl := range docLens {
			mergedDocLens[docID] = dl
		}
	}

	merged.postings = mergedPostings
	merged.docLens = mergedDocLens
	merged.docCount = len(mergedDocLens)
	merged.flushed = true

	// Replace segments with merged + active
	sm.segments = []*Segment{merged}
	if active != nil {
		sm.segments = append(sm.segments, active)
	}

	sm.mu.Unlock()
}

// MergeQueue returns the merge notification channel.
func (sm *SegmentManager) MergeQueue() <-chan int {
	return sm.mergeQueue
}

// EncodePostings encodes a posting list using varint + delta compression.
// Format: [docID(varint), termFreq(varint), positionCount(varint), positions(delta,varint)...]
func EncodePostings(postings []Posting) []byte {
	if len(postings) == 0 {
		return nil
	}

	buf := make([]byte, 0, len(postings)*16)
	prevDocID := uint64(0)

	for _, p := range postings {
		// Delta encode docID
		buf = binary.AppendUvarint(buf, p.DocID-prevDocID)
		prevDocID = p.DocID

		// Term frequency
		buf = binary.AppendUvarint(buf, uint64(p.TermFreq))

		// Position count
		buf = binary.AppendUvarint(buf, uint64(len(p.Positions)))

		// Delta encode positions
		prevPos := 0
		for _, pos := range p.Positions {
			buf = binary.AppendUvarint(buf, uint64(pos-prevPos))
			prevPos = pos
		}
	}

	return buf
}

// DecodePostings decodes a posting list from varint + delta compression.
func DecodePostings(data []byte) []Posting {
	if len(data) == 0 {
		return nil
	}

	var postings []Posting
	offset := 0
	prevDocID := uint64(0)

	for offset < len(data) {
		// Delta decode docID
		delta, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			break
		}
		offset += n
		docID := prevDocID + delta
		prevDocID = docID

		// Term frequency
		tf, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			break
		}
		offset += n

		// Position count
		posCount, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			break
		}
		offset += n

		// Delta decode positions
		positions := make([]int, posCount)
		prevPos := 0
		for i := 0; i < int(posCount); i++ {
			deltaPos, n := binary.Uvarint(data[offset:])
			if n <= 0 {
				break
			}
			offset += n
			positions[i] = prevPos + int(deltaPos)
			prevPos = positions[i]
		}

		postings = append(postings, Posting{
			DocID:     docID,
			TermFreq:  int(tf),
			Positions: positions,
		})
	}

	return postings
}
