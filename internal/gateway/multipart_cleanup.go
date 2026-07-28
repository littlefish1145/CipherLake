package gateway

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// MultipartCleanup periodically removes expired multipart upload metadata and parts.
type MultipartCleanup struct {
	handler   *MultipartUploadHandler
	interval  time.Duration
	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

func NewMultipartCleanup(handler *MultipartUploadHandler, interval time.Duration) *MultipartCleanup {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &MultipartCleanup{
		handler:  handler,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

func (c *MultipartCleanup) Start() {
	c.startOnce.Do(func() {
		c.wg.Add(1)
		go c.run()
	})
}

func (c *MultipartCleanup) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
		c.wg.Wait()
	})
}

func (c *MultipartCleanup) run() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := c.handler.CleanupExpiredUploads(ctx)
			cancel()
			if err != nil {
				zap.L().Warn("failed to clean expired multipart uploads", zap.Error(err))
			}
		}
	}
}
