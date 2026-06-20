package units

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		{"GB", "1GB", 1 << 30, false},
		{"2GB", "2GB", 2 << 30, false},
		{"MB", "1MB", 1 << 20, false},
		{"KB", "1KB", 1 << 10, false},
		{"TB", "1TB", 1 << 40, false},
		{"100GB", "100GB", 100 * (1 << 30), false},
		{"512MB", "512MB", 512 * (1 << 20), false},
		{"lowercase gb", "10gb", 10 << 30, false},
		{"mixed case", "10Gb", 10 << 30, false},
		{"empty", "", 0, false},
		{"plain bytes", "100B", 100, false},
		{"with spaces", " 10 GB ", 10 << 30, false},
		{"missing number", "GB", 0, true},
		{"invalid unit", "1XB", 0, true},
		{"not a number", "abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSize(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0B"},
		{512, "512B"},
		{1 << 10, "1KB"},
		{1 << 20, "1MB"},
		{1 << 30, "1GB"},
		{1 << 40, "1TB"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, FormatSize(tt.bytes))
		})
	}
}
