package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

type TaskType string

const (
	TaskTieringDecision     TaskType = "tiering_decision"
	TaskScrubCheck          TaskType = "scrub_check"
	TaskKeyRotation         TaskType = "key_rotation"
	TaskMultipartCleanup    TaskType = "multipart_cleanup"
	TaskHotIndexEviction    TaskType = "hot_index_eviction"
	TaskColdIndexCompaction TaskType = "cold_index_compaction"
	TaskMetadataCompaction  TaskType = "metadata_compaction"
	TaskReplicationSync     TaskType = "replication_sync"
	TaskVersionCleanup      TaskType = "version_cleanup"
	TaskLifecycleProcess    TaskType = "lifecycle_process"
)

type TaskConfig struct {
	Type        TaskType
	Schedule    string
	Handler     TaskHandler
	Concurrency int
	RetryPolicy *RetryPolicy
	DependsOn   []TaskType
	Enabled     bool
}

type RetryPolicy struct {
	MaxRetries        int
	InitialDelay      time.Duration
	MaxDelay          time.Duration
	BackoffMultiplier float64
}

type TaskHandler func(ctx context.Context) error

type TaskResult struct {
	TaskType   TaskType
	Success    bool
	StartedAt  time.Time
	FinishedAt time.Time
	Error      error
	RetryCount int
	Output     string
}

type Scheduler struct {
	cron           *cron.Cron
	tasks          map[TaskType]*TaskConfig
	taskResults    map[TaskType][]*TaskResult
	taskResultsMu  sync.RWMutex
	runningTasks   map[TaskType]bool
	runningTasksMu sync.RWMutex
	mu             sync.RWMutex
	maxHistory     int
	disabled       map[TaskType]bool
	disabledMu     sync.RWMutex
	cronIDs        map[TaskType]cron.EntryID
}

func NewScheduler() *Scheduler {
	// Accept both standard 5-field cron and 6-field cron-with-seconds so
	// that callers (e.g. plugin loader manifest schedules) can use either.
	parser := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &Scheduler{
		cron:         cron.New(cron.WithParser(parser)),
		tasks:        make(map[TaskType]*TaskConfig),
		taskResults:  make(map[TaskType][]*TaskResult),
		runningTasks: make(map[TaskType]bool),
		maxHistory:   100,
		disabled:     make(map[TaskType]bool),
		cronIDs:      make(map[TaskType]cron.EntryID),
	}
}

func (s *Scheduler) RegisterTask(config *TaskConfig) (cron.EntryID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if config.Handler == nil {
		return 0, fmt.Errorf("handler is required for task %s", config.Type)
	}

	s.tasks[config.Type] = config

	// Schedule is optional: tasks without a schedule are on-demand-only and
	// invoked exclusively via RunTaskNow. They still need to be registered so
	// that RunTaskNow can look them up and respect their RetryPolicy.
	if config.Schedule == "" {
		return 0, nil
	}

	entryID, err := s.cron.AddFunc(config.Schedule, func() {
		s.executeTask(context.Background(), config.Type)
	})
	if err != nil {
		return 0, fmt.Errorf("failed to add task %s: %w", config.Type, err)
	}
	s.cronIDs[config.Type] = entryID

	return entryID, nil
}

// UnregisterTask removes a registered task from the scheduler and stops
// future cron invocations. In-flight executions are not interrupted.
func (s *Scheduler) UnregisterTask(taskType TaskType) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskType]; !ok {
		return fmt.Errorf("task %s not found", taskType)
	}

	if entryID, ok := s.cronIDs[taskType]; ok {
		s.cron.Remove(entryID)
		delete(s.cronIDs, taskType)
	}
	delete(s.tasks, taskType)
	return nil
}

func (s *Scheduler) Start() error {
	s.cron.Start()
	return nil
}

func (s *Scheduler) Stop() context.Context {
	return s.cron.Stop()
}

func (s *Scheduler) RunTaskNow(ctx context.Context, taskType TaskType) error {
	s.mu.RLock()
	_, exists := s.tasks[taskType]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("task %s not found", taskType)
	}

	return s.executeTask(ctx, taskType)
}

func (s *Scheduler) executeTask(ctx context.Context, taskType TaskType) error {
	s.mu.RLock()
	config, exists := s.tasks[taskType]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("task %s not found", taskType)
	}

	s.disabledMu.RLock()
	if s.disabled[taskType] {
		s.disabledMu.RUnlock()
		return nil
	}
	s.disabledMu.RUnlock()

	s.runningTasksMu.Lock()
	if s.runningTasks[taskType] {
		if config.Concurrency <= 1 {
			s.runningTasksMu.Unlock()
			return fmt.Errorf("task %s is already running", taskType)
		}
	}
	s.runningTasks[taskType] = true
	s.runningTasksMu.Unlock()

	defer func() {
		s.runningTasksMu.Lock()
		delete(s.runningTasks, taskType)
		s.runningTasksMu.Unlock()
	}()

	result := &TaskResult{
		TaskType:  taskType,
		StartedAt: time.Now(),
	}

	var err error
	retryPolicy := &RetryPolicy{
		MaxRetries:        1,
		InitialDelay:      time.Second,
		MaxDelay:          time.Minute,
		BackoffMultiplier: 2,
	}
	if config.RetryPolicy != nil {
		retryPolicy = config.RetryPolicy
	}

	for attempt := 0; attempt <= retryPolicy.MaxRetries; attempt++ {
		result.RetryCount = attempt

		if attempt > 0 {
			delay := retryPolicy.InitialDelay
			if retryPolicy.BackoffMultiplier > 0 {
				for i := 0; i < attempt; i++ {
					delay = time.Duration(float64(delay) * retryPolicy.BackoffMultiplier)
					if delay > retryPolicy.MaxDelay {
						delay = retryPolicy.MaxDelay
					}
				}
			}
			if retryErr := waitForRetry(ctx, delay); retryErr != nil {
				err = retryErr
				break
			}
		}

		err = config.Handler(ctx)
		if err == nil {
			break
		}
	}

	result.FinishedAt = time.Now()
	if err != nil {
		result.Success = false
		result.Error = err
	} else {
		result.Success = true
	}

	s.recordResult(taskType, result)

	return err
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Scheduler) recordResult(taskType TaskType, result *TaskResult) {
	s.taskResultsMu.Lock()
	defer s.taskResultsMu.Unlock()

	s.taskResults[taskType] = append(s.taskResults[taskType], result)

	if len(s.taskResults[taskType]) > s.maxHistory {
		s.taskResults[taskType] = s.taskResults[taskType][len(s.taskResults[taskType])-s.maxHistory:]
	}
}

func (s *Scheduler) GetTaskHistory(taskType TaskType, limit int) []*TaskResult {
	s.taskResultsMu.RLock()
	defer s.taskResultsMu.RUnlock()

	results := s.taskResults[taskType]
	if limit <= 0 || limit > len(results) {
		limit = len(results)
	}

	result := make([]*TaskResult, limit)
	copy(result, results[len(results)-limit:])
	return result
}

func (s *Scheduler) GetAllTaskHistory() map[TaskType][]*TaskResult {
	s.taskResultsMu.RLock()
	defer s.taskResultsMu.RUnlock()

	result := make(map[TaskType][]*TaskResult)
	for k, v := range s.taskResults {
		result[k] = v
	}
	return result
}

func (s *Scheduler) IsTaskRunning(taskType TaskType) bool {
	s.runningTasksMu.RLock()
	defer s.runningTasksMu.RUnlock()
	return s.runningTasks[taskType]
}

func (s *Scheduler) DisableTask(taskType TaskType) {
	s.disabledMu.Lock()
	defer s.disabledMu.Unlock()
	s.disabled[taskType] = true
}

func (s *Scheduler) EnableTask(taskType TaskType) {
	s.disabledMu.Lock()
	defer s.disabledMu.Unlock()
	delete(s.disabled, taskType)
}

func (s *Scheduler) GetTaskStatus() map[TaskType]*TaskStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	s.taskResultsMu.RLock()
	defer s.taskResultsMu.RUnlock()

	s.runningTasksMu.RLock()
	defer s.runningTasksMu.RUnlock()

	status := make(map[TaskType]*TaskStatus)
	for taskType, config := range s.tasks {
		history := s.taskResults[taskType]
		var lastRun *TaskResult
		if len(history) > 0 {
			lastRun = history[len(history)-1]
		}

		status[taskType] = &TaskStatus{
			Type:        taskType,
			Enabled:     !s.disabled[taskType],
			Schedule:    config.Schedule,
			IsRunning:   s.runningTasks[taskType],
			LastRun:     lastRun,
			SuccessRate: s.calculateSuccessRate(history),
		}
	}

	return status
}

func (s *Scheduler) calculateSuccessRate(results []*TaskResult) float64 {
	if len(results) == 0 {
		return 0
	}

	successes := 0
	for _, r := range results {
		if r.Success {
			successes++
		}
	}

	return float64(successes) / float64(len(results))
}

type TaskStatus struct {
	Type        TaskType
	Enabled     bool
	Schedule    string
	IsRunning   bool
	LastRun     *TaskResult
	SuccessRate float64
}

type TaskStats struct {
	TotalRuns   int
	Successes   int
	Failures    int
	AvgDuration time.Duration
	MaxRetries  int
}

func (s *Scheduler) GetTaskStats(taskType TaskType) *TaskStats {
	s.taskResultsMu.RLock()
	defer s.taskResultsMu.RUnlock()

	results := s.taskResults[taskType]
	stats := &TaskStats{
		TotalRuns: len(results),
	}

	var totalDuration time.Duration
	for _, r := range results {
		if r.Success {
			stats.Successes++
		} else {
			stats.Failures++
		}

		if r.RetryCount > stats.MaxRetries {
			stats.MaxRetries = r.RetryCount
		}

		if !r.FinishedAt.IsZero() && !r.StartedAt.IsZero() {
			totalDuration += r.FinishedAt.Sub(r.StartedAt)
		}
	}

	if stats.TotalRuns > 0 {
		stats.AvgDuration = totalDuration / time.Duration(stats.TotalRuns)
	}

	return stats
}

type TaskBuilder struct {
	scheduler *Scheduler
	config    *TaskConfig
}

func NewTaskBuilder(scheduler *Scheduler, taskType TaskType) *TaskBuilder {
	return &TaskBuilder{
		scheduler: scheduler,
		config: &TaskConfig{
			Type:        taskType,
			Enabled:     true,
			Concurrency: 1,
		},
	}
}

func (tb *TaskBuilder) Schedule(schedule string) *TaskBuilder {
	tb.config.Schedule = schedule
	return tb
}

func (tb *TaskBuilder) Handler(handler TaskHandler) *TaskBuilder {
	tb.config.Handler = handler
	return tb
}

func (tb *TaskBuilder) Concurrency(concurrency int) *TaskBuilder {
	tb.config.Concurrency = concurrency
	return tb
}

func (tb *TaskBuilder) Retry(maxRetries int, initialDelay, maxDelay time.Duration, multiplier float64) *TaskBuilder {
	tb.config.RetryPolicy = &RetryPolicy{
		MaxRetries:        maxRetries,
		InitialDelay:      initialDelay,
		MaxDelay:          maxDelay,
		BackoffMultiplier: multiplier,
	}
	return tb
}

func (tb *TaskBuilder) DependsOn(tasks ...TaskType) *TaskBuilder {
	tb.config.DependsOn = tasks
	return tb
}

func (tb *TaskBuilder) Register() error {
	_, err := tb.scheduler.RegisterTask(tb.config)
	return err
}

func DefaultTaskConfigs() map[TaskType]*TaskConfig {
	return map[TaskType]*TaskConfig{
		TaskTieringDecision: {
			Type:        TaskTieringDecision,
			Schedule:    "0 */6 * * *",
			Concurrency: 1,
			RetryPolicy: &RetryPolicy{
				MaxRetries:        3,
				InitialDelay:      1 * time.Minute,
				MaxDelay:          10 * time.Minute,
				BackoffMultiplier: 2.0,
			},
		},
		TaskScrubCheck: {
			Type:        TaskScrubCheck,
			Schedule:    "0 2 * * 0",
			Concurrency: 4,
			RetryPolicy: &RetryPolicy{
				MaxRetries:        2,
				InitialDelay:      5 * time.Minute,
				MaxDelay:          30 * time.Minute,
				BackoffMultiplier: 3.0,
			},
		},
		TaskKeyRotation: {
			Type:        TaskKeyRotation,
			Schedule:    "0 3 1 * *",
			Concurrency: 1,
			RetryPolicy: &RetryPolicy{
				MaxRetries:        1,
				InitialDelay:      10 * time.Minute,
				MaxDelay:          1 * time.Hour,
				BackoffMultiplier: 2.0,
			},
		},
		TaskMultipartCleanup: {
			Type:        TaskMultipartCleanup,
			Schedule:    "0 */1 * * *",
			Concurrency: 2,
			RetryPolicy: &RetryPolicy{
				MaxRetries:        1,
				InitialDelay:      30 * time.Second,
				MaxDelay:          5 * time.Minute,
				BackoffMultiplier: 2.0,
			},
		},
		TaskHotIndexEviction: {
			Type:        TaskHotIndexEviction,
			Schedule:    "*/10 * * * *",
			Concurrency: 2,
		},
		TaskColdIndexCompaction: {
			Type:        TaskColdIndexCompaction,
			Schedule:    "0 4 * * 0",
			Concurrency: 1,
			RetryPolicy: &RetryPolicy{
				MaxRetries:        1,
				InitialDelay:      1 * time.Hour,
				MaxDelay:          6 * time.Hour,
				BackoffMultiplier: 2.0,
			},
		},
		TaskLifecycleProcess: {
			Type:        TaskLifecycleProcess,
			Schedule:    "0 5 * * *",
			Concurrency: 4,
		},
		TaskVersionCleanup: {
			Type:        TaskVersionCleanup,
			Schedule:    "0 6 1 * *",
			Concurrency: 2,
		},
	}
}
