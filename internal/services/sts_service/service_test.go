package sts_service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSTSService_StopCleanupLoopIdempotent verifies that calling
// StopCleanupLoop multiple times does not panic (closing an already-closed
// channel would panic without sync.Once protection).
func TestSTSService_StopCleanupLoopIdempotent(t *testing.T) {
	svc := NewSTSService(nil)
	svc.StartCleanupLoop(1 * time.Hour)

	require.NotPanics(t, func() {
		svc.StopCleanupLoop()
		svc.StopCleanupLoop()
		svc.StopCleanupLoop()
	})
}

// TestSTSService_StopCleanupLoopWithoutStart verifies that calling
// StopCleanupLoop without a prior StartCleanupLoop returns promptly
// (WaitGroup should be 0).
func TestSTSService_StopCleanupLoopWithoutStart(t *testing.T) {
	svc := NewSTSService(nil)

	done := make(chan struct{})
	go func() {
		svc.StopCleanupLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopCleanupLoop without Start did not return within 2 seconds")
	}
}

// TestSTSService_StopWaitsForGoroutine verifies that StopCleanupLoop blocks
// until the cleanup goroutine has actually exited.
func TestSTSService_StopWaitsForGoroutine(t *testing.T) {
	svc := NewSTSService(nil)
	// Use a long interval so CleanupExpired (which needs a real iamService)
	// never fires during the test — we are only verifying goroutine exit.
	svc.StartCleanupLoop(1 * time.Hour)

	// Give the loop a chance to start.
	time.Sleep(30 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		svc.StopCleanupLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopCleanupLoop did not return within 5 seconds")
	}
}

// TestSTSService_NewService verifies basic construction.
func TestSTSService_NewService(t *testing.T) {
	svc := NewSTSService(nil)
	require.NotNil(t, svc)
	assert.NotNil(t, svc.stopCh)
}
