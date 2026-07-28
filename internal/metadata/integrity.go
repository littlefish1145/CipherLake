package metadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type ChecksumType string

const (
	ChecksumNone   ChecksumType = ""
	ChecksumCRC32C ChecksumType = "CRC32C"
	ChecksumCRC64  ChecksumType = "CRC64"
	ChecksumSHA256 ChecksumType = "SHA256"
)

type ChecksumResult struct {
	Type     ChecksumType
	Value    string
	Computed time.Time
}

type IntegrityChecker struct {
	mu          sync.RWMutex
	enabled     bool
	defaultType ChecksumType
	scrubConfig *ScrubConfig
	scrubState  *ScrubState
	stats       *IntegrityStats
}

type ScrubConfig struct {
	Enabled      bool
	Interval     time.Duration
	BatchSize    int
	StorageTiers []int
	Parallelism  int
	OnCorruption CorruptHandler
}

type CorruptHandler func(ctx context.Context, bucket, key string, err error) error

type ScrubState struct {
	mu              sync.RWMutex
	InProgress      bool
	StartedAt       time.Time
	LastRunAt       time.Time
	LastCompletedAt time.Time
	ObjectsChecked  int64
	ObjectsCorrupt  int64
	Errors          int64
	CurrentBucket   string
	CurrentKey      string
}

type IntegrityStats struct {
	ChecksPerformed     int64
	ChecksFailed        int64
	CorruptionsDetected int64
	CorruptionsRepaired int64
	LastScrubAt         time.Time
}

type IntegrityReader struct {
	reader   io.Reader
	checksum hash.Hash
	result   *ChecksumResult
	typ      ChecksumType
}

func NewIntegrityChecker(config *ScrubConfig) *IntegrityChecker {
	if config == nil {
		config = &ScrubConfig{
			Enabled:     true,
			Interval:    30 * 24 * time.Hour,
			BatchSize:   1000,
			Parallelism: 4,
		}
	}

	return &IntegrityChecker{
		enabled:     config.Enabled,
		defaultType: ChecksumCRC32C,
		scrubConfig: config,
		scrubState: &ScrubState{
			ObjectsChecked: 0,
			ObjectsCorrupt: 0,
			Errors:         0,
		},
		stats: &IntegrityStats{},
	}
}

func (ic *IntegrityChecker) ComputeChecksum(data []byte, typ ChecksumType) string {
	if typ == "" {
		typ = ic.defaultType
	}

	switch typ {
	case ChecksumCRC32C:
		return fmt.Sprintf("%08x", crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
	case ChecksumCRC64:
		return fmt.Sprintf("%016x", crc64.Checksum(data, crc64.MakeTable(crc64.ECMA)))
	case ChecksumSHA256:
		h := sha256.New()
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil))
	default:
		return fmt.Sprintf("%08x", crc32.ChecksumIEEE(data))
	}
}

func (ic *IntegrityChecker) NewIntegrityReader(r io.Reader, typ ChecksumType) *IntegrityReader {
	if typ == "" {
		typ = ic.defaultType
	}

	var h hash.Hash
	switch typ {
	case ChecksumCRC32C:
		h = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case ChecksumCRC64:
		h = crc64.New(crc64.MakeTable(crc64.ECMA))
	case ChecksumSHA256:
		h = sha256.New()
	default:
		h = crc32.New(crc32.IEEETable)
	}

	return &IntegrityReader{
		reader:   r,
		checksum: h,
		typ:      typ,
	}
}

func (ir *IntegrityReader) Read(p []byte) (int, error) {
	n, err := ir.reader.Read(p)
	if n > 0 {
		ir.checksum.Write(p[:n])
	}
	return n, err
}

func (ir *IntegrityReader) Result() *ChecksumResult {
	return &ChecksumResult{
		Type:     ir.typ,
		Value:    hex.EncodeToString(ir.checksum.Sum(nil)),
		Computed: time.Now(),
	}
}

func (ic *IntegrityChecker) Verify(data []byte, expected string, typ ChecksumType) (bool, error) {
	if typ == "" {
		typ = ic.defaultType
	}

	computed := ic.ComputeChecksum(data, typ)

	if computed != expected {
		return false, &ChecksumMismatchError{
			Expected:     expected,
			Computed:     computed,
			ChecksumType: typ,
		}
	}

	return true, nil
}

type ChecksumMismatchError struct {
	Expected     string
	Computed     string
	ChecksumType ChecksumType
}

func (e *ChecksumMismatchError) Error() string {
	return fmt.Sprintf("checksum mismatch: expected %s, computed %s (type: %s)", e.Expected, e.Computed, e.ChecksumType)
}

type IntegrityWriter struct {
	writer   io.Writer
	checksum hash.Hash
	result   *ChecksumResult
	typ      ChecksumType
}

func NewIntegrityWriter(w io.Writer, typ ChecksumType) *IntegrityWriter {
	if typ == "" {
		typ = ChecksumCRC32C
	}

	var h hash.Hash
	switch typ {
	case ChecksumCRC32C:
		h = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case ChecksumCRC64:
		h = crc64.New(crc64.MakeTable(crc64.ECMA))
	case ChecksumSHA256:
		h = sha256.New()
	default:
		h = crc32.New(crc32.IEEETable)
	}

	return &IntegrityWriter{
		writer:   io.MultiWriter(w, h),
		checksum: h,
		typ:      typ,
	}
}

func (iw *IntegrityWriter) Write(p []byte) (int, error) {
	return iw.writer.Write(p)
}

func (iw *IntegrityWriter) Result() *ChecksumResult {
	return &ChecksumResult{
		Type:     iw.typ,
		Value:    hex.EncodeToString(iw.checksum.Sum(nil)),
		Computed: time.Now(),
	}
}

func (ic *IntegrityChecker) GetScrubState() *ScrubState {
	ic.scrubState.mu.RLock()
	defer ic.scrubState.mu.RUnlock()

	return &ScrubState{
		InProgress:      ic.scrubState.InProgress,
		StartedAt:       ic.scrubState.StartedAt,
		LastRunAt:       ic.scrubState.LastRunAt,
		LastCompletedAt: ic.scrubState.LastCompletedAt,
		ObjectsChecked:  atomic.LoadInt64(&ic.scrubState.ObjectsChecked),
		ObjectsCorrupt:  atomic.LoadInt64(&ic.scrubState.ObjectsCorrupt),
		Errors:          atomic.LoadInt64(&ic.scrubState.Errors),
		CurrentBucket:   ic.scrubState.CurrentBucket,
		CurrentKey:      ic.scrubState.CurrentKey,
	}
}

func (ic *IntegrityChecker) GetStats() *IntegrityStats {
	ic.mu.RLock()
	defer ic.mu.RUnlock()

	return &IntegrityStats{
		ChecksPerformed:     atomic.LoadInt64(&ic.stats.ChecksPerformed),
		ChecksFailed:        atomic.LoadInt64(&ic.stats.ChecksFailed),
		CorruptionsDetected: atomic.LoadInt64(&ic.stats.CorruptionsDetected),
		CorruptionsRepaired: atomic.LoadInt64(&ic.stats.CorruptionsRepaired),
		LastScrubAt:         ic.stats.LastScrubAt,
	}
}

type Scrubber interface {
	Start(ctx context.Context) error
	Stop() error
	ScrubBucket(ctx context.Context, bucket string, objectLister ObjectLister, storageReader StorageReader) error
}

type ObjectLister interface {
	ListObjects(ctx context.Context, bucket string, maxKeys int) ([]ObjectInfo, error)
}

type ObjectInfo struct {
	Key          string
	StorageTier  int
	Size         int64
	Checksum     string
	ChecksumType ChecksumType
}

type StorageReader interface {
	ReadObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	RepairObject(ctx context.Context, bucket, key string) error
}

type BackgroundScrubber struct {
	checker   *IntegrityChecker
	config    *ScrubConfig
	stopCh    chan struct{}
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewBackgroundScrubber(checker *IntegrityChecker, config *ScrubConfig) *BackgroundScrubber {
	return &BackgroundScrubber{
		checker: checker,
		config:  config,
		stopCh:  make(chan struct{}),
	}
}

func (bs *BackgroundScrubber) Start(ctx context.Context) error {
	if !bs.config.Enabled {
		return nil
	}

	bs.startOnce.Do(func() {
		bs.wg.Add(1)
		go bs.runScrubLoop(ctx)
	})
	return nil
}

func (bs *BackgroundScrubber) Stop() error {
	bs.stopOnce.Do(func() {
		close(bs.stopCh)
		bs.wg.Wait()
	})
	return nil
}

func (bs *BackgroundScrubber) runScrubLoop(ctx context.Context) {
	defer bs.wg.Done()

	ticker := time.NewTicker(bs.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-bs.stopCh:
			return
		case <-ticker.C:
			bs.checker.scrubState.mu.Lock()
			bs.checker.scrubState.LastRunAt = time.Now()
			bs.checker.scrubState.mu.Unlock()
		}
	}
}

func (bs *BackgroundScrubber) ScrubBucket(ctx context.Context, bucket string, objectLister ObjectLister, storageReader StorageReader) error {
	bs.checker.scrubState.mu.Lock()
	bs.checker.scrubState.InProgress = true
	bs.checker.scrubState.StartedAt = time.Now()
	bs.checker.scrubState.CurrentBucket = bucket
	bs.checker.scrubState.mu.Unlock()

	defer func() {
		bs.checker.scrubState.mu.Lock()
		bs.checker.scrubState.InProgress = false
		bs.checker.scrubState.LastCompletedAt = time.Now()
		bs.checker.scrubState.mu.Unlock()
	}()

	offset := 0
	batchSize := bs.config.BatchSize

	for {
		objects, err := objectLister.ListObjects(ctx, bucket, batchSize)
		if err != nil {
			atomic.AddInt64(&bs.checker.scrubState.Errors, 1)
			return fmt.Errorf("failed to list objects: %w", err)
		}

		if len(objects) == 0 {
			break
		}

		sem := make(chan struct{}, bs.config.Parallelism)
		var wg sync.WaitGroup

		for _, obj := range objects {
			if !bs.shouldScrubTier(obj.StorageTier) {
				continue
			}

			sem <- struct{}{}
			wg.Add(1)

			go func(o ObjectInfo) {
				defer wg.Done()
				defer func() { <-sem }()

				bs.scrubObject(ctx, o, storageReader)
			}(obj)
		}

		wg.Wait()
		offset += batchSize
	}

	return nil
}

func (bs *BackgroundScrubber) shouldScrubTier(tier int) bool {
	if len(bs.config.StorageTiers) == 0 {
		return true
	}

	for _, t := range bs.config.StorageTiers {
		if t == tier {
			return true
		}
	}
	return false
}

func (bs *BackgroundScrubber) scrubObject(ctx context.Context, obj ObjectInfo, storageReader StorageReader) {
	bs.checker.scrubState.mu.Lock()
	bs.checker.scrubState.CurrentKey = obj.Key
	bs.checker.scrubState.mu.Unlock()

	reader, err := storageReader.ReadObject(ctx, "", obj.Key)
	if err != nil {
		atomic.AddInt64(&bs.checker.scrubState.Errors, 1)
		return
	}
	defer reader.Close()

	ir := bs.checker.NewIntegrityReader(reader, obj.ChecksumType)

	buf := make([]byte, 32*1024)
	for {
		_, err := io.ReadFull(ir, buf)
		if err == io.EOF {
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			atomic.AddInt64(&bs.checker.scrubState.Errors, 1)
			return
		}
	}

	result := ir.Result()
	atomic.AddInt64(&bs.checker.scrubState.ObjectsChecked, 1)
	atomic.AddInt64(&bs.checker.stats.ChecksPerformed, 1)

	if result.Value != obj.Checksum {
		atomic.AddInt64(&bs.checker.scrubState.ObjectsCorrupt, 1)
		atomic.AddInt64(&bs.checker.stats.CorruptionsDetected, 1)

		if bs.config.OnCorruption != nil {
			if err := bs.config.OnCorruption(ctx, "", obj.Key, fmt.Errorf("checksum mismatch")); err != nil {
				atomic.AddInt64(&bs.checker.scrubState.Errors, 1)
			}
		}
	}
}

type RepairHandler struct {
	mu         sync.RWMutex
	repairLog  []RepairRecord
	maxRecords int
}

type RepairRecord struct {
	ID          string    `json:"id"`
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	OldChecksum string    `json:"old_checksum"`
	NewChecksum string    `json:"new_checksum"`
	RepairedAt  time.Time `json:"repaired_at"`
	Success     bool      `json:"success"`
	Error       string    `json:"error,omitempty"`
}

func NewRepairHandler(maxRecords int) *RepairHandler {
	if maxRecords <= 0 {
		maxRecords = 10000
	}
	return &RepairHandler{
		repairLog:  make([]RepairRecord, 0, maxRecords),
		maxRecords: maxRecords,
	}
}

func (rh *RepairHandler) RecordRepair(record RepairRecord) {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	if record.ID == "" {
		record.ID = uuid.New().String()
	}
	record.RepairedAt = time.Now()

	rh.repairLog = append(rh.repairLog, record)

	if len(rh.repairLog) > rh.maxRecords {
		rh.repairLog = rh.repairLog[len(rh.repairLog)-rh.maxRecords:]
	}
}

func (rh *RepairHandler) GetRepairHistory(limit int) []RepairRecord {
	rh.mu.RLock()
	defer rh.mu.RUnlock()

	if limit <= 0 || limit > len(rh.repairLog) {
		limit = len(rh.repairLog)
	}

	result := make([]RepairRecord, limit)
	copy(result, rh.repairLog[len(rh.repairLog)-limit:])
	return result
}

type ValidatingReader struct {
	reader       io.Reader
	checker      *IntegrityChecker
	expected     string
	checksumType ChecksumType
}

func NewValidatingReader(r io.Reader, expected string, typ ChecksumType, checker *IntegrityChecker) *ValidatingReader {
	return &ValidatingReader{
		reader:       r,
		checker:      checker,
		expected:     expected,
		checksumType: typ,
	}
}

func (vr *ValidatingReader) Read(p []byte) (int, error) {
	n, err := vr.reader.Read(p)
	if n > 0 && vr.expected != "" {
		if vr.checksumType == "" {
			vr.checksumType = ChecksumCRC32C
		}

		checksum := vr.checker.ComputeChecksum(p[:n], vr.checksumType)
		if checksum != vr.expected {
			return n, &ChecksumMismatchError{
				Expected:     vr.expected,
				Computed:     checksum,
				ChecksumType: vr.checksumType,
			}
		}
	}
	return n, err
}
