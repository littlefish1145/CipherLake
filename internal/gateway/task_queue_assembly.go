package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"cipherlake/internal/config"
	"cipherlake/internal/taskqueue"
)

type gatewayTaskQueueHandlers struct {
	vectorize taskqueue.Handler
	fts       taskqueue.Handler
	pipeline  taskqueue.Handler
}

func newGatewayTaskQueue(cfg *config.Config, handlers gatewayTaskQueueHandlers) (*taskqueue.Queue, error) {
	if !cfg.TaskQueue.Enabled {
		return nil, nil
	}

	store, err := taskqueue.NewBoltStore(gatewayTaskStorePath(cfg))
	if err != nil {
		return nil, fmt.Errorf("failed to create task store: %w", err)
	}

	queue := taskqueue.NewQueue(store, gatewayTaskWorkers(cfg), gatewayTaskQueueOptions(cfg)...)
	if handlers.vectorize != nil {
		queue.Register(taskqueue.KindVectorize, handlers.vectorize)
	}
	if handlers.fts != nil {
		queue.Register(taskqueue.KindFTS, handlers.fts)
	}
	if handlers.pipeline != nil {
		queue.Register(taskqueue.KindPipeline, handlers.pipeline)
	}

	queue.Start()
	return queue, nil
}

func gatewayTaskStorePath(cfg *config.Config) string {
	storePath := cfg.TaskQueue.StorePath
	if storePath == "" {
		storePath = filepath.Join(cfg.Node.DataDir, "tasks.db")
	}
	if !filepath.IsAbs(storePath) {
		storePath = filepath.Join(cfg.Node.DataDir, storePath)
	}
	return storePath
}

func gatewayTaskWorkers(cfg *config.Config) int {
	if cfg.TaskQueue.Workers > 0 {
		return cfg.TaskQueue.Workers
	}
	return 4
}

func gatewayTaskQueueOptions(cfg *config.Config) []taskqueue.QueueOption {
	return []taskqueue.QueueOption{
		taskqueue.WithPollInterval(parseDuration(cfg.TaskQueue.PollInterval, 500*time.Millisecond)),
		taskqueue.WithRetryBase(parseDuration(cfg.TaskQueue.RetryBase, time.Second)),
		taskqueue.WithMaxRetryDelay(parseDuration(cfg.TaskQueue.MaxRetryDelay, 5*time.Minute)),
	}
}

func stopGatewayTaskQueue(ctx context.Context, queue *taskqueue.Queue) error {
	if queue == nil {
		return nil
	}
	return queue.Stop(ctx)
}
