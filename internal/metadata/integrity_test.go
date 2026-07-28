package metadata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type integrityTestStorageReader struct {
	data []byte
}

func (r integrityTestStorageReader) ReadObject(context.Context, string, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(r.data)), nil
}

func (integrityTestStorageReader) RepairObject(context.Context, string, string) error {
	return nil
}

func TestChecksumType(t *testing.T) {
	assert.Equal(t, ChecksumType(""), ChecksumNone)
	assert.Equal(t, ChecksumType("CRC32C"), ChecksumCRC32C)
	assert.Equal(t, ChecksumType("CRC64"), ChecksumCRC64)
	assert.Equal(t, ChecksumType("SHA256"), ChecksumSHA256)
}

func TestNewIntegrityChecker(t *testing.T) {
	checker := NewIntegrityChecker(nil)
	assert.NotNil(t, checker)
	assert.True(t, checker.enabled)
}

func TestIntegrityChecker_ComputeChecksum(t *testing.T) {
	checker := NewIntegrityChecker(&ScrubConfig{Enabled: true})

	tests := []struct {
		name     string
		data     []byte
		checksum ChecksumType
	}{
		{"CRC32C empty", []byte{}, ChecksumCRC32C},
		{"CRC32C content", []byte("hello world"), ChecksumCRC32C},
		{"CRC64 empty", []byte{}, ChecksumCRC64},
		{"CRC64 content", []byte("hello world"), ChecksumCRC64},
		{"SHA256 empty", []byte{}, ChecksumSHA256},
		{"SHA256 content", []byte("hello world"), ChecksumSHA256},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checker.ComputeChecksum(tt.data, tt.checksum)
			assert.NotEmpty(t, result)
		})
	}
}

func TestIntegrityChecker_ComputeChecksum_Consistency(t *testing.T) {
	checker := NewIntegrityChecker(&ScrubConfig{Enabled: true})
	data := []byte("test data for consistency check")

	result1 := checker.ComputeChecksum(data, ChecksumCRC32C)
	result2 := checker.ComputeChecksum(data, ChecksumCRC32C)

	assert.Equal(t, result1, result2)
}

func TestIntegrityChecker_DifferentData(t *testing.T) {
	checker := NewIntegrityChecker(&ScrubConfig{Enabled: true})
	data1 := []byte("first data")
	data2 := []byte("second data")

	result1 := checker.ComputeChecksum(data1, ChecksumCRC32C)
	result2 := checker.ComputeChecksum(data2, ChecksumCRC32C)

	assert.NotEqual(t, result1, result2)
}

func TestScrubConfig(t *testing.T) {
	config := &ScrubConfig{
		Enabled:     true,
		Interval:    24 * 3600 * 1000000000,
		BatchSize:   100,
		Parallelism: 4,
	}

	assert.True(t, config.Enabled)
	assert.Equal(t, 100, config.BatchSize)
	assert.Equal(t, 4, config.Parallelism)
}

func TestScrubState(t *testing.T) {
	state := &ScrubState{
		InProgress:     true,
		ObjectsChecked: 1000,
		ObjectsCorrupt: 5,
		Errors:         10,
	}

	assert.True(t, state.InProgress)
	assert.Equal(t, int64(1000), state.ObjectsChecked)
	assert.Equal(t, int64(5), state.ObjectsCorrupt)
	assert.Equal(t, int64(10), state.Errors)
}

func TestIntegrityStats(t *testing.T) {
	stats := &IntegrityStats{
		ChecksPerformed:     10000,
		ChecksFailed:        50,
		CorruptionsDetected: 5,
		CorruptionsRepaired: 3,
	}

	assert.Equal(t, int64(10000), stats.ChecksPerformed)
	assert.Equal(t, int64(50), stats.ChecksFailed)
	assert.Equal(t, int64(5), stats.CorruptionsDetected)
	assert.Equal(t, int64(3), stats.CorruptionsRepaired)
}

func TestBackgroundScrubber_RecordsCorruptionCallbackFailure(t *testing.T) {
	config := &ScrubConfig{
		Enabled: true,
		OnCorruption: func(context.Context, string, string, error) error {
			return errors.New("callback failed")
		},
	}
	checker := NewIntegrityChecker(config)
	scrubber := NewBackgroundScrubber(checker, config)

	scrubber.scrubObject(context.Background(), ObjectInfo{
		Key:          "corrupt.txt",
		Checksum:     "not-the-computed-checksum",
		ChecksumType: ChecksumCRC32C,
	}, integrityTestStorageReader{data: []byte("content")})

	assert.Equal(t, int64(1), checker.scrubState.ObjectsChecked)
	assert.Equal(t, int64(1), checker.scrubState.ObjectsCorrupt)
	assert.Equal(t, int64(1), checker.scrubState.Errors)
}

// TestBackgroundScrubber_StartStopIdempotent verifies that Start() and Stop()
// can be called multiple times without panicking.
func TestBackgroundScrubber_StartStopIdempotent(t *testing.T) {
	config := &ScrubConfig{
		Enabled:  true,
		Interval: 1 * time.Second,
	}
	checker := NewIntegrityChecker(config)
	scrubber := NewBackgroundScrubber(checker, config)

	require.NoError(t, scrubber.Start(context.Background()))
	// Calling Start again should be a no-op.
	require.NoError(t, scrubber.Start(context.Background()))

	require.NoError(t, scrubber.Stop())
	// Calling Stop again should be a no-op.
	assert.NotPanics(t, func() {
		_ = scrubber.Stop()
		_ = scrubber.Stop()
	})
}

// TestBackgroundScrubber_DisabledDoesNotStart verifies that a disabled scrubber
// does not start a goroutine and Stop is a no-op.
func TestBackgroundScrubber_DisabledDoesNotStart(t *testing.T) {
	config := &ScrubConfig{Enabled: false}
	checker := NewIntegrityChecker(config)
	scrubber := NewBackgroundScrubber(checker, config)

	require.NoError(t, scrubber.Start(context.Background()))
	// Stop should still be safe even though no goroutine was started.
	require.NoError(t, scrubber.Stop())
}

func BenchmarkIntegrityChecker_ComputeChecksum_CRC32C(b *testing.B) {
	data := make([]byte, 1024*1024)
	checker := NewIntegrityChecker(nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		checker.ComputeChecksum(data, ChecksumCRC32C)
	}
}

func BenchmarkIntegrityChecker_ComputeChecksum_SHA256(b *testing.B) {
	data := make([]byte, 1024*1024)
	checker := NewIntegrityChecker(nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		checker.ComputeChecksum(data, ChecksumSHA256)
	}
}
