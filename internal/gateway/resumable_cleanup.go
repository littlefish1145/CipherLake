package gateway

import (
	"context"
	"os"
	"sync"
	"time"
)

// ResumableCleanup runs a background goroutine that periodically cleans up
// expired resumable upload sessions.
type ResumableCleanup struct {
	handler  *ResumableUploadHandler
	interval time.Duration
	stopCh   chan struct{}
	stopped  bool
	startOnce sync.Once
}

// NewResumableCleanup creates a new cleanup manager.
func NewResumableCleanup(handler *ResumableUploadHandler, interval time.Duration) *ResumableCleanup {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	return &ResumableCleanup{
		handler:  handler,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the background cleanup loop.
func (c *ResumableCleanup) Start() {
	c.startOnce.Do(func() {
		go c.run()
	})
}

// Stop signals the cleanup loop to exit.
func (c *ResumableCleanup) Stop() {
	if !c.stopped {
		c.stopped = true
		close(c.stopCh)
	}
}

func (c *ResumableCleanup) run() {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

func (c *ResumableCleanup) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessions, err := c.handler.gateway.metadata.ListExpiredSessions(ctx)
	if err != nil {
		return
	}

	for _, session := range sessions {
		// Delete temp file
		tempFilePath := c.handler.getTempFilePath(session.UploadID)
		os.Remove(tempFilePath)

		// Delete session metadata
		c.handler.gateway.metadata.DeleteResumableSession(ctx, session.UploadID)

		// Record metrics
		RecordSessionExpired()
	}
}
