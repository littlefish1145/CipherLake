package storage

import "fmt"

// ErasureConfig holds configuration for erasure-coded storage.
type ErasureConfig struct {
	// DataShards is the number of data shards (k). Default is 4.
	DataShards int
	// ParityShards is the number of parity shards (m). Default is 2.
	ParityShards int
	// Backends are the underlying storage backends (must be >= k+m).
	Backends []BackendStorage
	// ShardMapping maps an object path and shard index to the shard path
	// stored in the corresponding backend. If nil, a default mapping is used.
	ShardMapping func(path string, shardIdx int) string
}

// DefaultShardMapping provides the default shard path mapping:
// <original_path>.shard.<idx>
func DefaultShardMapping(path string, shardIdx int) string {
	return fmt.Sprintf("%s.shard.%d", path, shardIdx)
}
