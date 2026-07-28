// Package units provides size/duration parsing helpers used across CipherLake.
package units

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize parses human-readable size strings like "10GB", "512MB", "1TB"
// into bytes. An empty string returns 0 without error. Units are case-insensitive
// and must be one of B, KB, MB, GB, TB.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	upper := strings.ToUpper(s)
	var multiplier int64 = 1
	var numeric string

	switch {
	case strings.HasSuffix(upper, "TB"):
		multiplier = 1 << 40
		numeric = s[:len(s)-2]
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1 << 30
		numeric = s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1 << 20
		numeric = s[:len(s)-2]
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1 << 10
		numeric = s[:len(s)-2]
	case strings.HasSuffix(upper, "B"):
		multiplier = 1
		numeric = s[:len(s)-1]
	default:
		numeric = s
	}

	numeric = strings.TrimSpace(numeric)
	if numeric == "" {
		return 0, fmt.Errorf("invalid size string %q: missing numeric value", s)
	}

	value, err := strconv.ParseInt(numeric, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size string %q: %w", s, err)
	}

	return value * multiplier, nil
}

// FormatSize converts bytes into a human-readable string.
func FormatSize(bytes int64) string {
	switch {
	case bytes >= 1<<40:
		return fmt.Sprintf("%dTB", bytes/(1<<40))
	case bytes >= 1<<30:
		return fmt.Sprintf("%dGB", bytes/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%dMB", bytes/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%dKB", bytes/(1<<10))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
