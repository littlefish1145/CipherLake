package gateway

import (
	"fmt"
	"path/filepath"

	"cipherlake/internal/common"
	"cipherlake/internal/config"
	"cipherlake/internal/metadata"
	"cipherlake/internal/storage"
)

type gatewayStores struct {
	metadata *metadata.BoltDBMetadataStore
	store    *storage.TieredObjectStore
}

func newGatewayStores(cfg *config.Config) (*gatewayStores, error) {
	metadataStore, err := metadata.NewBoltDBMetadataStore(filepath.Join(cfg.Node.DataDir, "metadata.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create metadata store: %w", err)
	}

	store := storage.NewTieredObjectStore(metadataStore)
	if err := registerGatewayStorageTiers(store, cfg); err != nil {
		metadataStore.Close()
		return nil, err
	}

	return &gatewayStores{
		metadata: metadataStore,
		store:    store,
	}, nil
}

func registerGatewayStorageTiers(store *storage.TieredObjectStore, cfg *config.Config) error {
	tiers := []struct {
		tier     common.StorageTier
		path     string
		maxBytes int64
	}{
		{
			tier:     common.TierHot,
			path:     filepath.Join(cfg.Node.DataDir, "hot"),
			maxBytes: cfg.Tiering.HotMaxBytes,
		},
		{
			tier:     common.TierWarm,
			path:     filepath.Join(cfg.Node.DataDir, "warm"),
			maxBytes: cfg.Tiering.WarmMaxBytes(),
		},
		{
			tier:     common.TierCold,
			path:     filepath.Join(cfg.Node.DataDir, "cold"),
			maxBytes: cfg.Tiering.ColdMaxBytes(),
		},
		{
			tier:     common.TierArchive,
			path:     archiveStoragePath(cfg),
			maxBytes: cfg.Tiering.ArchiveMaxBytes(),
		},
	}

	for _, tier := range tiers {
		backend, err := storage.NewFileBackend(tier.path)
		if err != nil {
			return fmt.Errorf("failed to create %s storage backend: %w", tier.tier.String(), err)
		}
		store.RegisterTier(tier.tier, backend, tier.maxBytes)
	}
	return nil
}

func archiveStoragePath(cfg *config.Config) string {
	if cfg.Tiering.ArchivePath != "" {
		return cfg.Tiering.ArchivePath
	}
	return filepath.Join(cfg.Node.DataDir, "archive")
}
