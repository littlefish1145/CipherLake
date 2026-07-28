package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ResumableCleanup runs a background goroutine that periodically cleans up
// expired resumable upload sessions.
type ResumableCleanup struct {
	handler   *ResumableUploadHandler
	interval  time.Duration
	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
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
		c.wg.Add(1)
		go c.run()
	})
}

// Stop signals the cleanup loop to exit.
func (c *ResumableCleanup) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
		c.wg.Wait()
	})
}

func (c *ResumableCleanup) run() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			if err := c.cleanup(); err != nil {
				zap.L().Warn("failed to clean expired resumable uploads", zap.Error(err))
			}
		}
	}
}

func (c *ResumableCleanup) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessions, err := c.handler.gateway.metadata.ListExpiredSessions(ctx)
	if err != nil {
		return fmt.Errorf("list expired resumable sessions: %w", err)
	}

	var cleanupErr error
	for _, session := range sessions {
		tempFilePath := c.handler.getTempFilePath(session.UploadID)
		if err := os.Remove(tempFilePath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete resumable upload file %s: %w", session.UploadID, err))
			continue
		}

		if err := c.handler.gateway.metadata.DeleteResumableSession(ctx, session.UploadID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete resumable upload metadata %s: %w", session.UploadID, err))
			continue
		}

		RecordSessionExpired()
	}

	return cleanupErr
}
