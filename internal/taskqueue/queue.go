package taskqueue

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Queue schedules background tasks with retry, dead-letter, and worker-pool
// semantics. It persists task state so work can survive process restarts.
type Queue struct {
	store       Store
	handlers    map[string]Handler
	workers     int
	pollInterval time.Duration
	retryBase   time.Duration
	maxRetryDelay time.Duration

	mu        sync.RWMutex
	started   atomic.Bool
	stopCh    chan struct{}
	wg        sync.WaitGroup

	metrics   queueMetrics
}

type queueMetrics struct {
	submitted    atomic.Int64
	completed    atomic.Int64
	failed       atomic.Int64
	retried      atomic.Int64
	deadLettered atomic.Int64
}

// QueueOption configures the queue.
type QueueOption func(*Queue)

// NewQueue creates a task queue backed by store.
func NewQueue(store Store, workers int, opts ...QueueOption) *Queue {
	if workers <= 0 {
		workers = 1
	}
	q := &Queue{
		store:         store,
		handlers:      make(map[string]Handler),
		workers:       workers,
		pollInterval:  500 * time.Millisecond,
		retryBase:     1 * time.Second,
		maxRetryDelay: 5 * time.Minute,
		stopCh:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// WithPollInterval sets how frequently workers poll the store for new tasks.
func WithPollInterval(d time.Duration) QueueOption {
	return func(q *Queue) {
		if d > 0 {
			q.pollInterval = d
		}
	}
}

// WithRetryBase sets the base delay for exponential backoff.
func WithRetryBase(d time.Duration) QueueOption {
	return func(q *Queue) {
		if d > 0 {
			q.retryBase = d
		}
	}
}

// WithMaxRetryDelay caps the exponential backoff delay.
func WithMaxRetryDelay(d time.Duration) QueueOption {
	return func(q *Queue) {
		if d > 0 {
			q.maxRetryDelay = d
		}
	}
}

// Register binds a handler to a task kind. Submitting a task with an
// unregistered kind is a no-op that moves it to the dead-letter queue.
func (q *Queue) Register(kind string, handler Handler) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handlers[kind] = handler
}

// Submit adds a task to the queue. If the queue has started, the task will be
// picked up by a worker; otherwise it remains persisted until Start is called.
func (q *Queue) Submit(ctx context.Context, task *Task) error {
	if err := task.Validate(); err != nil {
		return err
	}
	if err := q.store.Put(ctx, task); err != nil {
		return fmt.Errorf("failed to persist task: %w", err)
	}
	q.metrics.submitted.Add(1)
	return nil
}

// Start launches the worker pool. It is safe to call multiple times.
func (q *Queue) Start() {
	if q.started.Swap(true) {
		return
	}
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.runWorker(i)
	}
}

// Stop drains active work and stops polling. Blocks until workers exit or the
// context deadline is reached.
func (q *Queue) Stop(ctx context.Context) error {
	if !q.started.Swap(false) {
		return nil
	}
	close(q.stopCh)

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) runWorker(id int) {
	defer q.wg.Done()
	for {
		select {
		case <-q.stopCh:
			return
		default:
		}

		if !q.processOne(context.Background()) {
			select {
			case <-q.stopCh:
				return
			case <-time.After(q.pollInterval):
			}
		}
	}
}

// processOne attempts to execute a single pending task. It returns true if a
// task was processed (regardless of success) and false when no pending tasks
// exist.
func (q *Queue) processOne(ctx context.Context) bool {
	pending, err := q.store.GetPending(ctx, 1)
	if err != nil {
		zap.L().Error("task queue failed to fetch pending tasks", zap.Error(err))
		return false
	}
	if len(pending) == 0 {
		return false
	}

	task := pending[0]
	if task.IsExpired() {
		task.Status = StatusDeadLetter
		task.Error = "deadline exceeded"
		task.Attempts++
		if err := q.store.Update(ctx, task); err != nil {
			zap.L().Error("task queue failed to mark expired task", zap.String("task_id", task.ID), zap.Error(err))
		}
		q.metrics.deadLettered.Add(1)
		return true
	}

	task.Status = StatusProcessing
	task.Attempts++
	if err := q.store.Update(ctx, task); err != nil {
		zap.L().Error("task queue failed to mark task processing", zap.String("task_id", task.ID), zap.Error(err))
		return true
	}

	handler, ok := q.handlerFor(task.Kind)
	if !ok {
		task.Status = StatusDeadLetter
		task.Error = fmt.Sprintf("no handler registered for kind %q", task.Kind)
		if err := q.store.Update(ctx, task); err != nil {
			zap.L().Error("task queue failed to dead-letter task", zap.String("task_id", task.ID), zap.Error(err))
		}
		q.metrics.deadLettered.Add(1)
		return true
	}

	err = handler(ctx, task)
	if err == nil {
		task.Status = StatusCompleted
		task.Error = ""
		if delErr := q.store.Delete(ctx, task.ID); delErr != nil {
			// If deletion fails, keep the completed record for observability.
			task.Error = fmt.Sprintf("completed but cleanup failed: %v", delErr)
			if updateErr := q.store.Update(ctx, task); updateErr != nil {
				zap.L().Error("task queue failed to update completed task", zap.String("task_id", task.ID), zap.Error(updateErr))
			}
		}
		q.metrics.completed.Add(1)
		return true
	}

	q.metrics.failed.Add(1)
	if task.Attempts >= task.MaxAttempts {
		task.Status = StatusDeadLetter
		task.Error = err.Error()
		if updateErr := q.store.Update(ctx, task); updateErr != nil {
			zap.L().Error("task queue failed to move task to dead letter", zap.String("task_id", task.ID), zap.Error(updateErr))
		}
		q.metrics.deadLettered.Add(1)
		zap.L().Warn("task moved to dead letter queue",
			zap.String("task_id", task.ID),
			zap.String("kind", task.Kind),
			zap.Int("attempts", task.Attempts),
			zap.Error(err))
	} else {
		task.Status = StatusPending
		task.Error = err.Error()
		if updateErr := q.store.Update(ctx, task); updateErr != nil {
			zap.L().Error("task queue failed to schedule retry", zap.String("task_id", task.ID), zap.Error(updateErr))
		}
		q.metrics.retried.Add(1)
		backoff := q.retryDelay(task.Attempts)
		time.Sleep(backoff)
	}
	return true
}

func (q *Queue) handlerFor(kind string) (Handler, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	h, ok := q.handlers[kind]
	return h, ok
}

func (q *Queue) retryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	delay := q.retryBase * time.Duration(1<<(attempt-1))
	if delay > q.maxRetryDelay {
		delay = q.maxRetryDelay
	}
	return delay
}

// Stats returns current queue counters.
func (q *Queue) Stats() map[string]int64 {
	return map[string]int64{
		"submitted":     q.metrics.submitted.Load(),
		"completed":     q.metrics.completed.Load(),
		"failed":        q.metrics.failed.Load(),
		"retried":       q.metrics.retried.Load(),
		"dead_lettered": q.metrics.deadLettered.Load(),
	}
}

// IsStarted reports whether the queue has been started.
func (q *Queue) IsStarted() bool {
	return q.started.Load()
}
