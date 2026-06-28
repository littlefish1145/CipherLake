package storage

import (
	"fmt"

	"nexus/internal/config"
)

// NewBackendFromConfig creates a BackendStorage from a StorageClassConfig.
func NewBackendFromConfig(cfg config.StorageClassConfig) (BackendStorage, error) {
	switch cfg.BackendType {
	case "file", "":
		if cfg.BackendPath == "" {
			return nil, fmt.Errorf("file backend requires backend_path")
		}
		return NewFileBackend(cfg.BackendPath)

	case "s3":
		return NewS3Backend(S3Config{
			Endpoint:       cfg.S3Endpoint,
			Region:         cfg.S3Region,
			Bucket:         cfg.S3Bucket,
			AccessKey:      cfg.S3AccessKey,
			SecretKey:      cfg.S3SecretKey,
			ForcePathStyle: cfg.S3ForcePathStyle,
		})

	case "azure":
		return NewAzureBlobBackend(AzureConfig{
			AccountName: cfg.AzureAccountName,
			AccountKey:  cfg.AzureAccountKey,
			Container:   cfg.AzureContainer,
			Endpoint:    cfg.AzureEndpoint,
		})

	case "erasure":
		return newErasureFromConfig(cfg)

	default:
		return nil, fmt.Errorf("unknown backend type: %s", cfg.BackendType)
	}
}

// newErasureFromConfig creates an ErasureCodedBackend from config.
// For erasure coding, the StorageClassConfig's sub-storage-classes are used
// as the underlying backends. If no sub-classes are specified, file backends
// are created under BackendPath with shard subdirectories.
func newErasureFromConfig(cfg config.StorageClassConfig) (*ErasureCodedBackend, error) {
	k := cfg.ErasureDataShards
	m := cfg.ErasureParityShards
	if k <= 0 {
		k = 4
	}
	if m <= 0 {
		m = 2
	}

	totalShards := k + m
	backends := make([]BackendStorage, totalShards)

	// Create file-based backends for each shard under BackendPath
	for i := 0; i < totalShards; i++ {
		shardPath := cfg.BackendPath
		if shardPath == "" {
			shardPath = fmt.Sprintf("data/erasure/shard%d", i)
		} else {
			shardPath = fmt.Sprintf("%s/shard%d", shardPath, i)
		}
		backend, err := NewFileBackend(shardPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create shard backend %d: %w", i, err)
		}
		backends[i] = backend
	}

	return NewErasureCodedBackend(ErasureConfig{
		DataShards:   k,
		ParityShards: m,
		Backends:     backends,
	})
}
