package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/reedsolomon"
)

// ErasureCodedBackend implements BackendStorage with Reed-Solomon erasure coding.
// Data is split into k data shards and m parity shards, then distributed across
// multiple underlying backends. The system can tolerate up to m backend failures.
type ErasureCodedBackend struct {
	mu           sync.RWMutex
	enc          reedsolomon.Encoder
	dataShards   int
	parityShards int
	backends     []BackendStorage
	shardMapping func(path string, shardIdx int) string
}

// NewErasureCodedBackend creates a new erasure-coded backend.
func NewErasureCodedBackend(cfg ErasureConfig) (*ErasureCodedBackend, error) {
	k := cfg.DataShards
	m := cfg.ParityShards
	if k <= 0 {
		k = 4
	}
	if m <= 0 {
		m = 2
	}

	if len(cfg.Backends) < k+m {
		return nil, fmt.Errorf("erasure coding requires at least %d backends, got %d", k+m, len(cfg.Backends))
	}

	enc, err := reedsolomon.New(k, m)
	if err != nil {
		return nil, fmt.Errorf("failed to create Reed-Solomon encoder: %w", err)
	}

	shardMapping := cfg.ShardMapping
	if shardMapping == nil {
		shardMapping = DefaultShardMapping
	}

	return &ErasureCodedBackend{
		enc:          enc,
		dataShards:   k,
		parityShards: m,
		backends:     cfg.Backends,
		shardMapping: shardMapping,
	}, nil
}

func (e *ErasureCodedBackend) Name() string {
	return "erasure"
}

func (e *ErasureCodedBackend) Put(ctx context.Context, path string, data io.Reader, size int64) error {
	allData, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("failed to read data for erasure encoding: %w", err)
	}
	return e.putShards(ctx, path, allData)
}

func (e *ErasureCodedBackend) putShards(ctx context.Context, path string, data []byte) error {
	// Prepend original data length so we can trim padding on read
	prefixedData := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(prefixedData[:8], uint64(len(data)))
	copy(prefixedData[8:], data)

	shards, err := e.splitAndEncode(prefixedData)
	if err != nil {
		return err
	}

	// Write all shards to their respective backends concurrently
	errCh := make(chan error, len(e.backends))
	var wg sync.WaitGroup

	totalShards := e.dataShards + e.parityShards
	for i := 0; i < totalShards && i < len(e.backends); i++ {
		wg.Add(1)
		go func(idx int, shardData []byte) {
			defer wg.Done()
			shardPath := e.shardMapping(path, idx)
			reader := bytes.NewReader(shardData)
			if err := e.backends[idx].Put(ctx, shardPath, reader, int64(len(shardData))); err != nil {
				errCh <- fmt.Errorf("failed to put shard %d: %w", idx, err)
				return
			}
			errCh <- nil
		}(i, shards[i])
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Allow up to parityShards failures
	if len(errs) > e.parityShards {
		return fmt.Errorf("too many shard write failures (%d): %v", len(errs), errs[0])
	}

	return nil
}

// splitAndEncode splits data into shards and computes parity.
func (e *ErasureCodedBackend) splitAndEncode(data []byte) ([][]byte, error) {
	shards, err := e.enc.Split(data)
	if err != nil {
		return nil, fmt.Errorf("failed to split data into shards: %w", err)
	}

	if err := e.enc.Encode(shards); err != nil {
		return nil, fmt.Errorf("failed to compute parity shards: %w", err)
	}

	return shards, nil
}

func (e *ErasureCodedBackend) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	data, err := e.getShards(ctx, path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (e *ErasureCodedBackend) getShards(ctx context.Context, path string) ([]byte, error) {
	totalShards := e.dataShards + e.parityShards
	shards := make([][]byte, totalShards)
	errs := make([]error, totalShards)

	// Read shards from all backends concurrently
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := 0; i < totalShards && i < len(e.backends); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			shardPath := e.shardMapping(path, idx)
			reader, err := e.backends[idx].Get(ctx, shardPath)
			if err != nil {
				mu.Lock()
				errs[idx] = err
				mu.Unlock()
				return
			}
			defer reader.Close()

			shardData, err := io.ReadAll(reader)
			if err != nil {
				mu.Lock()
				errs[idx] = err
				mu.Unlock()
				return
			}

			mu.Lock()
			shards[idx] = shardData
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// Count how many shards we successfully read
	available := 0
	for i := 0; i < totalShards; i++ {
		if errs[i] == nil && shards[i] != nil {
			available++
		}
	}

	if available < e.dataShards {
		return nil, fmt.Errorf("not enough shards available (%d/%d) to reconstruct object", available, e.dataShards)
	}

	// Mark missing shards as nil for reconstruction
	for i := 0; i < totalShards; i++ {
		if errs[i] != nil || shards[i] == nil {
			shards[i] = nil
		}
	}

	// Reconstruct if any shards are missing
	if available < totalShards {
		if err := e.enc.Reconstruct(shards); err != nil {
			return nil, fmt.Errorf("failed to reconstruct erasure-coded data: %w", err)
		}
	}

	// Verify data integrity
	if ok, err := e.enc.Verify(shards); !ok {
		return nil, fmt.Errorf("erasure-coded data verification failed: %v", err)
	}

	// Join data shards back together
	var buf bytes.Buffer
	if err := e.enc.Join(&buf, shards, len(shards[0])*e.dataShards); err != nil {
		return nil, fmt.Errorf("failed to join erasure shards: %w", err)
	}

	// Read the original data length from the first 8 bytes and trim padding
	joined := buf.Bytes()
	if len(joined) < 8 {
		return nil, fmt.Errorf("erasure-coded data too short to contain length prefix")
	}
	originalLen := binary.BigEndian.Uint64(joined[:8])
	if originalLen > uint64(len(joined)-8) {
		return nil, fmt.Errorf("erasure-coded data length prefix %d exceeds available data %d", originalLen, len(joined)-8)
	}
	return joined[8 : 8+originalLen], nil
}

func (e *ErasureCodedBackend) Delete(ctx context.Context, path string) error {
	totalShards := e.dataShards + e.parityShards
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < totalShards && i < len(e.backends); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			shardPath := e.shardMapping(path, idx)
			if err := e.backends[idx].Delete(ctx, shardPath); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("failed to delete shard %d: %w", idx, err))
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(errs) > e.parityShards {
		return fmt.Errorf("too many shard delete failures (%d): %v", len(errs), errs[0])
	}
	return nil
}

func (e *ErasureCodedBackend) Exists(ctx context.Context, path string) (bool, error) {
	// Check if at least k backends have the shard
	totalShards := e.dataShards + e.parityShards
	available := 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < totalShards && i < len(e.backends); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			shardPath := e.shardMapping(path, idx)
			exists, err := e.backends[idx].Exists(ctx, shardPath)
			if err == nil && exists {
				mu.Lock()
				available++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	return available >= e.dataShards, nil
}

func (e *ErasureCodedBackend) Size(ctx context.Context, path string) (int64, error) {
	// Check primary backend for size
	if len(e.backends) == 0 {
		return 0, ErrObjectNotFound
	}
	shardPath := e.shardMapping(path, 0)
	shardSize, err := e.backends[0].Size(ctx, shardPath)
	if err != nil {
		return 0, err
	}
	// Approximate original size from first shard size * dataShards
	// This is an approximation; actual size may be slightly different due to padding
	return shardSize * int64(e.dataShards), nil
}

func (e *ErasureCodedBackend) List(ctx context.Context, prefix string) ([]string, error) {
	// List from primary backend and strip shard suffix
	if len(e.backends) == 0 {
		return nil, nil
	}
	keys, err := e.backends[0].List(ctx, prefix)
	if err != nil {
		return nil, err
	}

	// Deduplicate by stripping shard suffix
	seen := make(map[string]bool)
	var result []string
	for _, key := range keys {
		basePath := stripShardSuffix(key)
		if !seen[basePath] && basePath != "" {
			seen[basePath] = true
			result = append(result, basePath)
		}
	}
	return result, nil
}

func (e *ErasureCodedBackend) PutReader(ctx context.Context, path string, reader io.Reader) (string, error) {
	allData, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("failed to read data for erasure encoding: %w", err)
	}
	if err := e.putShards(ctx, path, allData); err != nil {
		return "", err
	}
	etag := fmt.Sprintf(`"%x"`, len(allData))
	return etag, nil
}

func (e *ErasureCodedBackend) GetRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	// Simplified MVP: get full object then extract range
	data, err := e.getShards(ctx, path)
	if err != nil {
		return nil, err
	}

	if offset > int64(len(data)) {
		return nil, fmt.Errorf("offset beyond object size")
	}

	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}

	return io.NopCloser(bytes.NewReader(data[offset:end])), nil
}

func (e *ErasureCodedBackend) AtomicRename(ctx context.Context, oldPath, newPath string) error {
	totalShards := e.dataShards + e.parityShards
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < totalShards && i < len(e.backends); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			oldShardPath := e.shardMapping(oldPath, idx)
			newShardPath := e.shardMapping(newPath, idx)
			if err := e.backends[idx].AtomicRename(ctx, oldShardPath, newShardPath); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("failed to rename shard %d: %w", idx, err))
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(errs) > e.parityShards {
		return fmt.Errorf("too many shard rename failures (%d): %v", len(errs), errs[0])
	}
	return nil
}

func (e *ErasureCodedBackend) Close() error {
	var firstErr error
	for _, backend := range e.backends {
		if err := backend.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// stripShardSuffix removes the ".shard.N" suffix from a shard path
// to recover the original object path.
func stripShardSuffix(shardPath string) string {
	// Look for pattern ".shard.N" at the end
	for i := len(shardPath) - 1; i >= 0; i-- {
		if shardPath[i] == '.' {
			// Check if this is the ".shard.N" pattern
			suffix := shardPath[i:]
			if len(suffix) > 7 && suffix[:7] == ".shard." {
				return shardPath[:i]
			}
		}
	}
	return shardPath
}
