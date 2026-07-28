package config

import (
	"fmt"
	"strings"

	"cipherlake/internal/units"
)

// ValidationError represents a single validation failure or warning.
type ValidationError struct {
	Field    string `json:"field"`    // e.g., "vector.dim"
	Message  string `json:"message"`  // human-readable error description
	Expected string `json:"expected"` // e.g., "must be between 1 and 4096"
	Severity string `json:"severity"` // "error" or "warning"
}

func (e ValidationError) Error() string {
	if e.Expected != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Field, e.Message, e.Expected)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Validate checks a Config for schema compliance and returns all validation
// errors and warnings found. A non-empty result with any "error" severity
// means the config is invalid.
func Validate(cfg *Config) []ValidationError {
	var errs []ValidationError

	// 1. node.role
	validRoles := map[string]bool{"all": true, "gateway": true, "metadata": true, "storage": true}
	if !validRoles[cfg.Node.Role] {
		errs = append(errs, ValidationError{
			Field:    "node.role",
			Message:  fmt.Sprintf("invalid role %q", cfg.Node.Role),
			Expected: `must be one of: "all", "gateway", "metadata", "storage"`,
			Severity: "error",
		})
	}

	// 2. node.data_dir
	if cfg.Node.DataDir == "" {
		errs = append(errs, ValidationError{
			Field:    "node.data_dir",
			Message:  "data directory must not be empty",
			Expected: "must be a non-empty string",
			Severity: "error",
		})
	}

	// 3. node.listen_addr
	if !strings.HasPrefix(cfg.Node.ListenAddr, ":") {
		errs = append(errs, ValidationError{
			Field:    "node.listen_addr",
			Message:  fmt.Sprintf("invalid listen address %q", cfg.Node.ListenAddr),
			Expected: `must start with ":" (e.g., ":8080")`,
			Severity: "error",
		})
	}

	// 4. tiering.hot_max_size
	if cfg.Tiering.HotMaxSize != "" {
		if _, err := units.ParseSize(cfg.Tiering.HotMaxSize); err != nil {
			errs = append(errs, ValidationError{
				Field:    "tiering.hot_max_size",
				Message:  fmt.Sprintf("cannot parse size %q", cfg.Tiering.HotMaxSize),
				Expected: "must be a valid size string (e.g., 32GB, 1024MB)",
				Severity: "error",
			})
		}
	}

	// 5. vector.dim range
	if cfg.Vector.Dimension < 1 || cfg.Vector.Dimension > 4096 {
		errs = append(errs, ValidationError{
			Field:    "vector.dim",
			Message:  fmt.Sprintf("dimension %d is out of range", cfg.Vector.Dimension),
			Expected: "must be between 1 and 4096",
			Severity: "error",
		})
	}

	// 6. vector.dim should be 768 or a multiple of 64 (warning)
	if cfg.Vector.Dimension > 0 && cfg.Vector.Dimension != 768 && cfg.Vector.Dimension%64 != 0 {
		errs = append(errs, ValidationError{
			Field:    "vector.dim",
			Message:  fmt.Sprintf("dimension %d is not a common embedding dimension", cfg.Vector.Dimension),
			Expected: "should be 768 or a multiple of 64 for best compatibility",
			Severity: "warning",
		})
	}

	// 7. encryption.kms_type
	validKMS := map[string]bool{"": true, "local": true, "vault": true, "aws": true}
	if !validKMS[cfg.Encryption.KMSType] {
		errs = append(errs, ValidationError{
			Field:    "encryption.kms_type",
			Message:  fmt.Sprintf("invalid KMS type %q", cfg.Encryption.KMSType),
			Expected: `must be one of: "", "local", "vault", "aws"`,
			Severity: "error",
		})
	}

	// 8. crypto_services.audit_size
	if cfg.CryptoServices.AuditSize <= 0 {
		errs = append(errs, ValidationError{
			Field:    "crypto_services.audit_size",
			Message:  fmt.Sprintf("audit_size %d must be positive", cfg.CryptoServices.AuditSize),
			Expected: "must be greater than 0",
			Severity: "error",
		})
	}

	// 9. cache.ttl
	if cfg.Cache.TTL <= 0 {
		errs = append(errs, ValidationError{
			Field:    "cache.ttl",
			Message:  "cache TTL must be positive",
			Expected: "must be a positive duration (e.g., 5m, 300s)",
			Severity: "error",
		})
	}

	// 10. performance.max_upload_size
	if cfg.Performance.MaxUploadSize != "" {
		if _, err := units.ParseSize(cfg.Performance.MaxUploadSize); err != nil {
			errs = append(errs, ValidationError{
				Field:    "performance.max_upload_size",
				Message:  fmt.Sprintf("cannot parse size %q", cfg.Performance.MaxUploadSize),
				Expected: "must be a valid size string (e.g., 100GB)",
				Severity: "error",
			})
		}
	}

	// 11. tls.min_version
	validTLSVersions := map[string]bool{"1.0": true, "1.1": true, "1.2": true, "1.3": true}
	if cfg.TLS.MinVersion != "" && !validTLSVersions[cfg.TLS.MinVersion] {
		errs = append(errs, ValidationError{
			Field:    "tls.min_version",
			Message:  fmt.Sprintf("invalid TLS version %q", cfg.TLS.MinVersion),
			Expected: `must be one of: "1.0", "1.1", "1.2", "1.3"`,
			Severity: "error",
		})
	}

	// 12. ratelimit.global_rps must be > 0 if ratelimit.enabled
	if cfg.RateLimit.Enabled && cfg.RateLimit.GlobalRPS <= 0 {
		errs = append(errs, ValidationError{
			Field:    "ratelimit.global_rps",
			Message:  "global_rps must be positive when rate limiting is enabled",
			Expected: "must be greater than 0 when ratelimit.enabled is true",
			Severity: "error",
		})
	}

	// 13. auth.jwt_secret must be set if auth.require_auth is true
	if cfg.Auth.RequireAuth && cfg.Auth.JWTSecret == "" {
		errs = append(errs, ValidationError{
			Field:    "auth.jwt_secret",
			Message:  "jwt_secret must be set when authentication is required",
			Expected: "must be a non-empty string when auth.require_auth is true",
			Severity: "error",
		})
	}

	// 14. Cross-field: if vector.enabled, warn if dim is not a common embedding dimension
	if cfg.Vector.Enabled {
		commonDims := map[int]bool{768: true, 1024: true, 1536: true}
		if !commonDims[cfg.Vector.Dimension] {
			errs = append(errs, ValidationError{
				Field:    "vector.dim",
				Message:  fmt.Sprintf("dimension %d is not a common embedding model dimension", cfg.Vector.Dimension),
				Expected: "common dimensions are 768, 1024, 1536; verify this matches your embedding model",
				Severity: "warning",
			})
		}
	}

	// 15. Cross-field: if encryption.kms_type is "vault", vault_addr must be set
	if cfg.Encryption.KMSType == "vault" && cfg.Encryption.VaultAddr == "" {
		errs = append(errs, ValidationError{
			Field:    "encryption.vault_addr",
			Message:  "vault_addr must be set when using Vault KMS",
			Expected: "must be a non-empty string when encryption.kms_type is \"vault\"",
			Severity: "error",
		})
	}

	// 16. Cross-field: if encryption.kms_type is "aws", aws_kms_key_id must be set
	if cfg.Encryption.KMSType == "aws" && cfg.Encryption.AWSKMSKeyID == "" {
		errs = append(errs, ValidationError{
			Field:    "encryption.aws_kms_key_id",
			Message:  "aws_kms_key_id must be set when using AWS KMS",
			Expected: "must be a non-empty string when encryption.kms_type is \"aws\"",
			Severity: "error",
		})
	}

	return errs
}

// HasErrors returns true if any validation error has severity "error".
func HasErrors(errs []ValidationError) bool {
	for _, e := range errs {
		if e.Severity == "error" {
			return true
		}
	}
	return false
}
