package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewScheduler(t *testing.T) {
	sched := NewScheduler()
	assert.NotNil(t, sched)
	assert.NotNil(t, sched.tasks)
	assert.NotNil(t, sched.taskResults)
	assert.NotNil(t, sched.runningTasks)
}

func TestScheduler_Stop(t *testing.T) {
	sched := NewScheduler()

	sched.Start()
	time.Sleep(100 * time.Millisecond)

	ctx := sched.Stop()
	assert.NotNil(t, ctx)
}

func TestScheduler_RunTaskNow_NotFound(t *testing.T) {
	sched := NewScheduler()

	err := sched.RunTaskNow(nil, TaskTieringDecision)
	assert.Error(t, err)

	sched.Stop()
}

func TestScheduler_IsTaskRunning(t *testing.T) {
	sched := NewScheduler()

	assert.False(t, sched.IsTaskRunning(TaskKeyRotation))

	sched.Stop()
}

func TestRetryPolicy(t *testing.T) {
	policy := &RetryPolicy{
		MaxRetries:        3,
		InitialDelay:      1 * time.Second,
		MaxDelay:          60 * time.Second,
		BackoffMultiplier: 2.0,
	}

	assert.Equal(t, 3, policy.MaxRetries)
	assert.Equal(t, 1*time.Second, policy.InitialDelay)
	assert.Equal(t, 60*time.Second, policy.MaxDelay)
	assert.Equal(t, 2.0, policy.BackoffMultiplier)
}

func TestRunTaskNowCancellationInterruptsRetryBackoff(t *testing.T) {
	scheduler := NewScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	_, err := scheduler.RegisterTask(&TaskConfig{
		Type: "cancellable",
		Handler: func(context.Context) error {
			calls++
			cancel()
			return errors.New("expected failure")
		},
		RetryPolicy: &RetryPolicy{
			MaxRetries:   1,
			InitialDelay: time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("RegisterTask failed: %v", err)
	}

	err = scheduler.RunTaskNow(ctx, "cancellable")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTaskNow error = %v, want context canceled", err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
}

func TestTaskResult(t *testing.T) {
	result := &TaskResult{
		TaskType:   TaskScrubCheck,
		Success:    true,
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		RetryCount: 0,
	}

	assert.Equal(t, TaskScrubCheck, result.TaskType)
	assert.True(t, result.Success)
	assert.NotNil(t, result.StartedAt)
	assert.NotNil(t, result.FinishedAt)
}

func TestTaskConfig(t *testing.T) {
	config := &TaskConfig{
		Type:        TaskTieringDecision,
		Schedule:    "0 */6 * * * *",
		Concurrency: 2,
		Enabled:     true,
		DependsOn:   []TaskType{TaskScrubCheck},
	}

	assert.Equal(t, TaskTieringDecision, config.Type)
	assert.Equal(t, "0 */6 * * * *", config.Schedule)
	assert.Equal(t, 2, config.Concurrency)
	assert.True(t, config.Enabled)
	assert.Equal(t, 1, len(config.DependsOn))
}

func TestTaskType_Constants(t *testing.T) {
	assert.Equal(t, TaskType("tiering_decision"), TaskTieringDecision)
	assert.Equal(t, TaskType("scrub_check"), TaskScrubCheck)
	assert.Equal(t, TaskType("key_rotation"), TaskKeyRotation)
	assert.Equal(t, TaskType("multipart_cleanup"), TaskMultipartCleanup)
	assert.Equal(t, TaskType("hot_index_eviction"), TaskHotIndexEviction)
	assert.Equal(t, TaskType("cold_index_compaction"), TaskColdIndexCompaction)
}

func TestScheduler_DisableEnableTask(t *testing.T) {
	sched := NewScheduler()

	sched.DisableTask(TaskMultipartCleanup)
	sched.EnableTask(TaskMultipartCleanup)

	sched.Stop()
}
