package config

import (
	"os"
	"testing"

	"nexus/internal/units"

	"github.com/stretchr/testify/assert"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		hasError bool
	}{
		{"1GB", 1 << 30, false},
		{"2GB", 2 << 30, false},
		{"1MB", 1 << 20, false},
		{"1KB", 1 << 10, false},
		{"1TB", 1 << 40, false},
		{"100GB", 100 * (1 << 30), false},
		{"512MB", 512 * (1 << 20), false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := units.ParseSize(tt.input)
			if tt.hasError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestConfigNormalize(t *testing.T) {
	cfg := &Config{
		Tiering: TieringConfig{
			HotMaxSize: "32GB",
		},
		Cache: CacheConfig{
			MetadataMaxSize: "10GB",
			ObjectMaxSize:   "30GB",
		},
		Performance: PerformanceConfig{
			MaxUploadSize: "100GB",
		},
	}

	err := cfg.normalize()
	assert.NoError(t, err)

	assert.Equal(t, int64(32<<30), cfg.Tiering.HotMaxBytes)
	assert.Equal(t, int64(10<<30), cfg.Cache.MetadataMaxBytes)
	assert.Equal(t, int64(30<<30), cfg.Cache.ObjectMaxBytes)
	assert.Equal(t, int64(100<<30), cfg.Performance.MaxUploadBytes)
}

func TestLoadConfig(t *testing.T) {
	content := `
version: "2.0"
node:
  role: "all"
  listen_addr: ":8080"
  data_dir: "/tmp/nexus"
tiering:
  enabled: true
  hot_max_size: "16GB"
encryption:
  enable_dedup: true
vector:
  enabled: true
  dim: 768
`

	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	assert.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(content)
	assert.NoError(t, err)
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	assert.NoError(t, err)
	assert.NotNil(t, cfg)

	assert.Equal(t, "2.0", cfg.Version)
	assert.Equal(t, "all", cfg.Node.Role)
	assert.Equal(t, ":8080", cfg.Node.ListenAddr)
	assert.True(t, cfg.Tiering.Enabled)
	assert.True(t, cfg.Vector.Enabled)
	assert.True(t, cfg.Encryption.EnableDedup)
}
