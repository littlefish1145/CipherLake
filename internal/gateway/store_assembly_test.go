package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"cipherlake/internal/config"

	"github.com/stretchr/testify/assert"
)

func TestArchiveStoragePath(t *testing.T) {
	cfg := &config.Config{}
	cfg.Node.DataDir = t.TempDir()

	assert.Equal(t, filepath.Join(cfg.Node.DataDir, "archive"), archiveStoragePath(cfg))

	customArchive := filepath.Join(t.TempDir(), "cold-archive")
	cfg.Tiering.ArchivePath = customArchive

	assert.Equal(t, customArchive, archiveStoragePath(cfg))
}

func TestGatewayTaskQueueAssemblyDefaults(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Node.DataDir = dataDir

	assert.Equal(t, filepath.Join(dataDir, "tasks.db"), gatewayTaskStorePath(cfg))
	assert.Equal(t, 4, gatewayTaskWorkers(cfg))

	cfg.TaskQueue.StorePath = "custom/tasks.db"
	cfg.TaskQueue.Workers = 9

	assert.Equal(t, filepath.Join(dataDir, "custom", "tasks.db"), gatewayTaskStorePath(cfg))
	assert.Equal(t, 9, gatewayTaskWorkers(cfg))
}

func TestGatewayTaskStorePathAbsolute(t *testing.T) {
	cfg := &config.Config{}
	absolutePath := filepath.Join(t.TempDir(), "tasks.db")
	cfg.Node.DataDir = t.TempDir()
	cfg.TaskQueue.StorePath = absolutePath

	assert.Equal(t, absolutePath, gatewayTaskStorePath(cfg))
}

func TestGatewayAuthConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Auth.RequireAuth = true
	cfg.Auth.AnonymousRead = true
	cfg.Auth.JWTSecret = "secret"
	cfg.Auth.TokenExpiry = "15m"
	cfg.Auth.RefreshExpiry = "24h"

	authConfig := gatewayAuthConfig(cfg)

	assert.True(t, authConfig.RequireAuth)
	assert.True(t, authConfig.AnonymousRead)
	assert.Equal(t, "secret", authConfig.JWTSecret)
	assert.Equal(t, 15*time.Minute, authConfig.TokenExpiry)
	assert.Equal(t, 24*time.Hour, authConfig.RefreshExpiry)
}

func TestGatewayRateLimitConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.RateLimit.GlobalRPS = 100
	cfg.RateLimit.GlobalBurst = 20
	cfg.RateLimit.APILimits = map[string]config.APILimitConfig{
		"PUT": {RPS: 10, Burst: 2},
	}
	cfg.RateLimit.Whitelist = []string{"127.0.0.1"}

	limitConfig := gatewayRateLimitConfig(cfg)

	assert.EqualValues(t, 100, limitConfig.GlobalRPS)
	assert.EqualValues(t, 20, limitConfig.GlobalBurst)
	assert.EqualValues(t, 10, limitConfig.APILimits["PUT"].RPS)
	assert.EqualValues(t, 2, limitConfig.APILimits["PUT"].Burst)
	assert.Equal(t, []string{"127.0.0.1"}, limitConfig.Whitelist)
}
