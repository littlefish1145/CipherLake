package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func validConfig() *Config {
	return &Config{
		Version: "2.0",
		Node: NodeConfig{
			Role:         "all",
			ListenAddr:   ":8080",
			DataDir:      "/var/lib/nexus",
			ClusterPeers: []string{},
		},
		Tiering: TieringConfig{
			Enabled:    true,
			HotMaxSize: "32GB",
		},
		Encryption: EncryptionConfig{
			KMSType:     "local",
			MasterKeyPath: "/data/master.key",
		},
		CryptoServices: CryptoServicesConfig{
			AuditSize: 10000,
		},
		Vector: VectorConfig{
			Enabled:   false,
			Dimension: 768,
		},
		Cache: CacheConfig{
			TTL: 5 * time.Minute,
		},
		Performance: PerformanceConfig{
			MaxUploadSize: "100GB",
		},
		TLS: TLSConfig{
			MinVersion: "1.2",
		},
		RateLimit: RateLimitConfig{
			Enabled:   false,
			GlobalRPS: 1000,
		},
		Auth: AuthConfig{
			RequireAuth: false,
			JWTSecret:   "",
		},
	}
}

func TestValidateValidConfig(t *testing.T) {
	cfg := validConfig()
	errs := Validate(cfg)
	// A valid config should have no errors (warnings are ok)
	assert.False(t, HasErrors(errs), "valid config should have no errors: %v", errs)
}

func TestValidateNodeRole(t *testing.T) {
	tests := []struct {
		role      string
		wantError bool
	}{
		{"all", false},
		{"gateway", false},
		{"metadata", false},
		{"storage", false},
		{"invalid", true},
		{"", true},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			cfg := validConfig()
			cfg.Node.Role = tt.role
			errs := Validate(cfg)
			found := false
			for _, e := range errs {
				if e.Field == "node.role" && e.Severity == "error" {
					found = true
				}
			}
			assert.Equal(t, tt.wantError, found, "role=%q, wantError=%v, found=%v", tt.role, tt.wantError, found)
		})
	}
}

func TestValidateNodeDataDir(t *testing.T) {
	cfg := validConfig()
	cfg.Node.DataDir = ""
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "node.data_dir" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "empty data_dir should produce an error")
}

func TestValidateNodeListenAddr(t *testing.T) {
	tests := []struct {
		addr      string
		wantError bool
	}{
		{":8080", false},
		{":9090", false},
		{"0.0.0.0:8080", true},
		{"localhost:8080", true},
		{"", true},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			cfg := validConfig()
			cfg.Node.ListenAddr = tt.addr
			errs := Validate(cfg)
			found := false
			for _, e := range errs {
				if e.Field == "node.listen_addr" && e.Severity == "error" {
					found = true
				}
			}
			assert.Equal(t, tt.wantError, found, "addr=%q", tt.addr)
		})
	}
}

func TestValidateTieringHotMaxSize(t *testing.T) {
	cfg := validConfig()
	cfg.Tiering.HotMaxSize = "invalid"
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "tiering.hot_max_size" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "invalid hot_max_size should produce an error")
}

func TestValidateVectorDim(t *testing.T) {
	tests := []struct {
		name      string
		dim       int
		wantError bool
	}{
		{"768 valid", 768, false},
		{"1 valid", 1, false},
		{"4096 valid", 4096, false},
		{"0 invalid", 0, true},
		{"-1 invalid", -1, true},
		{"5000 invalid", 5000, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Vector.Dimension = tt.dim
			errs := Validate(cfg)
			found := false
			for _, e := range errs {
				if e.Field == "vector.dim" && e.Severity == "error" {
					found = true
				}
			}
			assert.Equal(t, tt.wantError, found, "dim=%d", tt.dim)
		})
	}
}

func TestValidateVectorDimWarning(t *testing.T) {
	tests := []struct {
		dim          int
		wantWarning  bool
	}{
		{768, false},  // 768 is common, no warning
		{1024, false}, // 1024 is a multiple of 64, no warning
		{512, false},  // 512 is a multiple of 64, no warning
		{100, true},   // not 768 and not multiple of 64
		{200, true},   // not 768 and not multiple of 64
		{64, false},   // multiple of 64
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			cfg := validConfig()
			cfg.Vector.Dimension = tt.dim
			errs := Validate(cfg)
			found := false
			for _, e := range errs {
				if e.Field == "vector.dim" && e.Severity == "warning" && e.Message != "" {
					// Check it's the "not a common embedding dimension" warning
					if contains(e.Message, "not a common embedding dimension") {
						found = true
					}
				}
			}
			assert.Equal(t, tt.wantWarning, found, "dim=%d", tt.dim)
		})
	}
}

func TestValidateKMSType(t *testing.T) {
	tests := []struct {
		kmsType   string
		wantError bool
	}{
		{"", false},
		{"local", false},
		{"vault", false},
		{"aws", false},
		{"gcp", true},
		{"invalid", true},
	}

	for _, tt := range tests {
		t.Run(tt.kmsType, func(t *testing.T) {
			cfg := validConfig()
			cfg.Encryption.KMSType = tt.kmsType
			// Set required fields for vault/aws to avoid cross-field errors
			if tt.kmsType == "vault" {
				cfg.Encryption.VaultAddr = "http://vault:8200"
			}
			if tt.kmsType == "aws" {
				cfg.Encryption.AWSKMSKeyID = "arn:aws:kms:..."
			}
			errs := Validate(cfg)
			found := false
			for _, e := range errs {
				if e.Field == "encryption.kms_type" && e.Severity == "error" {
					found = true
				}
			}
			assert.Equal(t, tt.wantError, found, "kmsType=%q", tt.kmsType)
		})
	}
}

func TestValidateCryptoServicesAuditSize(t *testing.T) {
	cfg := validConfig()
	cfg.CryptoServices.AuditSize = 0
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "crypto_services.audit_size" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "audit_size=0 should produce an error")

	cfg.CryptoServices.AuditSize = -1
	errs = Validate(cfg)
	found = false
	for _, e := range errs {
		if e.Field == "crypto_services.audit_size" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "negative audit_size should produce an error")
}

func TestValidateCacheTTL(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.TTL = 0
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "cache.ttl" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "zero TTL should produce an error")

	cfg.Cache.TTL = -1 * time.Second
	errs = Validate(cfg)
	found = false
	for _, e := range errs {
		if e.Field == "cache.ttl" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "negative TTL should produce an error")
}

func TestValidatePerformanceMaxUploadSize(t *testing.T) {
	cfg := validConfig()
	cfg.Performance.MaxUploadSize = "invalid"
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "performance.max_upload_size" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "invalid max_upload_size should produce an error")
}

func TestValidateTLSMinVersion(t *testing.T) {
	tests := []struct {
		version   string
		wantError bool
	}{
		{"1.0", false},
		{"1.1", false},
		{"1.2", false},
		{"1.3", false},
		{"2.0", true},
		{"0.9", true},
		{"", false}, // empty is ok (will use default)
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			cfg := validConfig()
			cfg.TLS.MinVersion = tt.version
			errs := Validate(cfg)
			found := false
			for _, e := range errs {
				if e.Field == "tls.min_version" && e.Severity == "error" {
					found = true
				}
			}
			assert.Equal(t, tt.wantError, found, "version=%q", tt.version)
		})
	}
}

func TestValidateRateLimitGlobalRPS(t *testing.T) {
	// When ratelimit is disabled, global_rps can be 0
	cfg := validConfig()
	cfg.RateLimit.Enabled = false
	cfg.RateLimit.GlobalRPS = 0
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "ratelimit.global_rps" && e.Severity == "error" {
			found = true
		}
	}
	assert.False(t, found, "global_rps=0 when disabled should not produce an error")

	// When ratelimit is enabled, global_rps must be > 0
	cfg.RateLimit.Enabled = true
	cfg.RateLimit.GlobalRPS = 0
	errs = Validate(cfg)
	found = false
	for _, e := range errs {
		if e.Field == "ratelimit.global_rps" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "global_rps=0 when enabled should produce an error")
}

func TestValidateAuthJWTSecret(t *testing.T) {
	// When require_auth is false, jwt_secret can be empty
	cfg := validConfig()
	cfg.Auth.RequireAuth = false
	cfg.Auth.JWTSecret = ""
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "auth.jwt_secret" && e.Severity == "error" {
			found = true
		}
	}
	assert.False(t, found, "empty jwt_secret when require_auth=false should not produce an error")

	// When require_auth is true, jwt_secret must be set
	cfg.Auth.RequireAuth = true
	cfg.Auth.JWTSecret = ""
	errs = Validate(cfg)
	found = false
	for _, e := range errs {
		if e.Field == "auth.jwt_secret" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "empty jwt_secret when require_auth=true should produce an error")
}

// Cross-field constraint tests

func TestValidateCrossFieldVaultAddr(t *testing.T) {
	cfg := validConfig()
	cfg.Encryption.KMSType = "vault"
	cfg.Encryption.VaultAddr = ""
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "encryption.vault_addr" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "empty vault_addr with kms_type=vault should produce an error")

	// With vault_addr set, no error
	cfg.Encryption.VaultAddr = "http://vault:8200"
	errs = Validate(cfg)
	found = false
	for _, e := range errs {
		if e.Field == "encryption.vault_addr" && e.Severity == "error" {
			found = true
		}
	}
	assert.False(t, found, "vault_addr set with kms_type=vault should not produce an error")
}

func TestValidateCrossFieldAWSKMSKeyID(t *testing.T) {
	cfg := validConfig()
	cfg.Encryption.KMSType = "aws"
	cfg.Encryption.AWSKMSKeyID = ""
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "encryption.aws_kms_key_id" && e.Severity == "error" {
			found = true
		}
	}
	assert.True(t, found, "empty aws_kms_key_id with kms_type=aws should produce an error")

	// With aws_kms_key_id set, no error
	cfg.Encryption.AWSKMSKeyID = "arn:aws:kms:us-east-1:123456789:key/abc"
	errs = Validate(cfg)
	found = false
	for _, e := range errs {
		if e.Field == "encryption.aws_kms_key_id" && e.Severity == "error" {
			found = true
		}
	}
	assert.False(t, found, "aws_kms_key_id set with kms_type=aws should not produce an error")
}

func TestValidateCrossFieldVectorEnabledDim(t *testing.T) {
	// When vector is enabled with dim=768, no warning for common dims
	cfg := validConfig()
	cfg.Vector.Enabled = true
	cfg.Vector.Dimension = 768
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "vector.dim" && e.Severity == "warning" && contains(e.Message, "not a common embedding model dimension") {
			found = true
		}
	}
	assert.False(t, found, "dim=768 when vector enabled should not produce a common-dim warning")

	// When vector is enabled with dim=500, should warn
	cfg.Vector.Dimension = 500
	errs = Validate(cfg)
	found = false
	for _, e := range errs {
		if e.Field == "vector.dim" && e.Severity == "warning" && contains(e.Message, "not a common embedding model dimension") {
			found = true
		}
	}
	assert.True(t, found, "dim=500 when vector enabled should produce a common-dim warning")

	// When vector is disabled with dim=500, no common-dim warning (only the multiple-of-64 warning)
	cfg.Vector.Enabled = false
	cfg.Vector.Dimension = 500
	errs = Validate(cfg)
	found = false
	for _, e := range errs {
		if e.Field == "vector.dim" && e.Severity == "warning" && contains(e.Message, "not a common embedding model dimension") {
			found = true
		}
	}
	assert.False(t, found, "dim=500 when vector disabled should not produce a common-dim warning")
}

// Hot reload tests

func TestIsHotReloadable(t *testing.T) {
	tests := []struct {
		field     string
		expected  bool
	}{
		{"logging.level", true},
		{"logging.format", true},
		{"ratelimit.enabled", true},
		{"ratelimit.global_rps", true},
		{"cache.ttl", true},
		{"events.enabled", true},
		{"events.workers", true},
		{"node.role", false},
		{"node.data_dir", false},
		{"encryption.kms_type", false},
		{"vector.enabled", false},
		{"tls.enabled", false},
		{"unknown.field", false},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsHotReloadable(tt.field))
		})
	}
}

func TestRequiresRestart(t *testing.T) {
	tests := []struct {
		field     string
		expected  bool
	}{
		{"node.role", true},
		{"node.listen_addr", true},
		{"encryption.kms_type", true},
		{"crypto_services.enabled", true},
		{"vector.enabled", true},
		{"tls.enabled", true},
		{"tls.min_version", true},
		{"logging.level", false},
		{"ratelimit.enabled", false},
		{"cache.ttl", false},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			assert.Equal(t, tt.expected, RequiresRestart(tt.field))
		})
	}
}

func TestDiffConfigs(t *testing.T) {
	oldCfg := validConfig()
	newCfg := validConfig()
	newCfg.Logging.Level = "debug"
	newCfg.Node.Role = "storage"
	newCfg.Cache.TTL = 10 * time.Minute

	changes := DiffConfigs(oldCfg, newCfg)

	changeMap := make(map[string]FieldChange)
	for _, c := range changes {
		changeMap[c.Field] = c
	}

	assert.Contains(t, changeMap, "logging.level", "logging.level change should be detected")
	assert.Contains(t, changeMap, "node.role", "node.role change should be detected")
	assert.Contains(t, changeMap, "cache.ttl", "cache.ttl change should be detected")

	// Check reloadability classification
	assert.True(t, changeMap["logging.level"].Reloadable, "logging.level should be reloadable")
	assert.False(t, changeMap["node.role"].Reloadable, "node.role should not be reloadable")
	assert.True(t, changeMap["cache.ttl"].Reloadable, "cache.ttl should be reloadable")
}

func TestApplyHotReload(t *testing.T) {
	oldCfg := validConfig()
	newCfg := validConfig()
	newCfg.Logging.Level = "debug"       // hot-reloadable
	newCfg.Node.Role = "storage"          // requires restart
	newCfg.Cache.TTL = 10 * time.Minute  // hot-reloadable
	newCfg.Encryption.KMSType = "vault"   // requires restart
	newCfg.Encryption.VaultAddr = "http://vault:8200" // requires restart

	merged, reloaded, skipped := ApplyHotReload(oldCfg, newCfg)

	// Hot-reloadable fields should be applied
	assert.Equal(t, "debug", merged.Logging.Level, "logging.level should be updated")
	assert.Equal(t, 10*time.Minute, merged.Cache.TTL, "cache.ttl should be updated")

	// Non-reloadable fields should remain from old config
	assert.Equal(t, "all", merged.Node.Role, "node.role should NOT be updated")
	assert.Equal(t, "local", merged.Encryption.KMSType, "encryption.kms_type should NOT be updated")

	// Check returned lists
	assert.Contains(t, reloaded, "logging.level")
	assert.Contains(t, reloaded, "cache.ttl")
	assert.Contains(t, skipped, "node.role")
}

func TestHasErrors(t *testing.T) {
	assert.False(t, HasErrors(nil))
	assert.False(t, HasErrors([]ValidationError{}))
	assert.False(t, HasErrors([]ValidationError{
		{Field: "test", Severity: "warning"},
	}))
	assert.True(t, HasErrors([]ValidationError{
		{Field: "test", Severity: "error"},
	}))
	assert.True(t, HasErrors([]ValidationError{
		{Field: "test1", Severity: "warning"},
		{Field: "test2", Severity: "error"},
	}))
}

// helper
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
