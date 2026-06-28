package fts

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// CompactionManager handles background compaction of FTS segments
// and enforces disk quota.
type CompactionManager struct {
	idx         *InvertedIndex
	maxIndexSize int64 // max index size in bytes
	running     int32
	stopCh      chan struct{}
	wg          sync.WaitGroup
	indexSize   int64 // current index size in bytes
}

// NewCompactionManager creates a new compaction manager.
func NewCompactionManager(idx *InvertedIndex, maxIndexSize int64) *CompactionManager {
	return &CompactionManager{
		idx:          idx,
		maxIndexSize: maxIndexSize,
		stopCh:       make(chan struct{}),
	}
}

// Start begins the background compaction goroutine.
func (cm *CompactionManager) Start() {
	if !atomic.CompareAndSwapInt32(&cm.running, 0, 1) {
		return
	}

	cm.wg.Add(1)
	go cm.compactionLoop()
}

// Stop halts the background compaction goroutine.
func (cm *CompactionManager) Stop() {
	if !atomic.CompareAndSwapInt32(&cm.running, 1, 0) {
		return
	}
	close(cm.stopCh)
	cm.wg.Wait()
}

// compactionLoop is the main background loop for compaction.
func (cm *CompactionManager) compactionLoop() {
	defer cm.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cm.stopCh:
			return
		case <-ticker.C:
			cm.runCompaction()
			cm.checkDiskQuota()
		case <-cm.idx.GetSegmentManager().MergeQueue():
			cm.runCompaction()
		}
	}
}

// runCompaction merges small segments and removes deleted documents.
func (cm *CompactionManager) runCompaction() {
	segManager := cm.idx.GetSegmentManager()
	if segManager.SegmentCount() <= 1 {
		return
	}

	// Merge segments
	segManager.MergeSegments()

	// Update term statistics after compaction
	cm.updateTermStats()
}

// updateTermStats recalculates term document frequencies after compaction.
func (cm *CompactionManager) updateTermStats() {
	cm.idx.mu.Lock()
	defer cm.idx.mu.Unlock()

	// Reset term DF
	newTermDF := make(map[string]int)

	// Recount from all segments
	segManager := cm.idx.GetSegmentManager()
	// We need to iterate through segments to count DF
	// For now, we rely on the existing termDF being approximately correct
	// A full rebuild would be done during major compaction
	for term := range cm.idx.termDF {
		postings := segManager.Search(term)
		if len(postings) > 0 {
			newTermDF[term] = len(postings)
		}
	}

	cm.idx.termDF = newTermDF

	// Update scorer stats
	docCount, avgDL := segManager.GetStats()
	cm.idx.scorer.UpdateStats(docCount, avgDL)
}

// checkDiskQuota checks if the index size exceeds the quota.
func (cm *CompactionManager) checkDiskQuota() {
	size, err := cm.getIndexSize()
	if err != nil {
		return
	}

	atomic.StoreInt64(&cm.indexSize, size)

	if cm.maxIndexSize > 0 && size > cm.maxIndexSize {
		fmt.Printf("[nexus-fts] WARNING: FTS index size %d bytes exceeds quota %d bytes. Indexing new documents will be rejected.\n",
			size, cm.maxIndexSize)
	}
}

// IsQuotaExceeded returns true if the disk quota has been exceeded.
func (cm *CompactionManager) IsQuotaExceeded() bool {
	if cm.maxIndexSize <= 0 {
		return false
	}
	size := atomic.LoadInt64(&cm.indexSize)
	return size > cm.maxIndexSize
}

// getIndexSize calculates the total size of the FTS index on disk.
func (cm *CompactionManager) getIndexSize() (int64, error) {
	dbPath := cm.idx.dbPath
	var totalSize int64

	err := filepath.Walk(filepath.Dir(dbPath), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})

	return totalSize, err
}

// GetIndexSizeBytes returns the current index size metric.
func (cm *CompactionManager) GetIndexSizeBytes() int64 {
	return atomic.LoadInt64(&cm.indexSize)
}

// SetMaxIndexSize updates the maximum index size quota.
func (cm *CompactionManager) SetMaxIndexSize(maxSize int64) {
	cm.maxIndexSize = maxSize
}

// ForceCompaction triggers an immediate compaction.
func (cm *CompactionManager) ForceCompaction() {
	cm.runCompaction()
}
