package taskqueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryStore_CRUD(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	task, err := NewTask("test", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}

	if err := store.Put(ctx, task); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	pending, err := store.GetPending(ctx, 10)
	if err != nil {
		t.Fatalf("GetPending failed: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending task, got %d", len(pending))
	}

	fetched, err := store.GetByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if fetched.ID != task.ID {
		t.Fatalf("expected task id %s, got %s", task.ID, fetched.ID)
	}

	if err := store.Delete(ctx, task.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	pending, err = store.GetPending(ctx, 10)
	if err != nil {
		t.Fatalf("GetPending after delete failed: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending tasks, got %d", len(pending))
	}
}

func TestQueue_SubmitAndProcess(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	q := NewQueue(store, 1, WithPollInterval(50*time.Millisecond))

	var counter atomic.Int32
	q.Register("inc", func(ctx context.Context, task *Task) error {
		counter.Add(1)
		return nil
	})

	q.Start()
	defer q.Stop(ctx)

	task, err := NewTask("inc", map[string]string{})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := q.Submit(ctx, task); err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	for i := 0; i < 50; i++ {
		if counter.Load() == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task was not processed")
}

func TestQueue_RetryAndDeadLetter(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	q := NewQueue(store, 1, WithPollInterval(50*time.Millisecond), WithRetryBase(10*time.Millisecond))

	var calls atomic.Int32
	q.Register("fail", func(ctx context.Context, task *Task) error {
		calls.Add(1)
		return errors.New("expected failure")
	})

	q.Start()
	defer q.Stop(ctx)

	task, err := NewTask("fail", map[string]string{}, WithRetry(2))
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := q.Submit(ctx, task); err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	if calls.Load() < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", calls.Load())
	}

	dead, err := store.ListDeadLetter(ctx, 10)
	if err != nil {
		t.Fatalf("ListDeadLetter failed: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("expected 1 dead-letter task, got %d", len(dead))
	}
}

func TestQueue_UnknownKindDeadLetter(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	q := NewQueue(store, 1, WithPollInterval(50*time.Millisecond))
	q.Start()
	defer q.Stop(ctx)

	task, err := NewTask("unknown", map[string]string{})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := q.Submit(ctx, task); err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	for i := 0; i < 50; i++ {
		dead, err := store.ListDeadLetter(ctx, 10)
		if err != nil {
			t.Fatalf("ListDeadLetter failed: %v", err)
		}
		if len(dead) == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("unknown task was not moved to dead letter")
}

func TestQueue_PriorityOrdering(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	order := make(chan string, 3)
	q := NewQueue(store, 1, WithPollInterval(50*time.Millisecond))
	q.Register("ordered", func(ctx context.Context, task *Task) error {
		order <- task.ID
		return nil
	})

	// Submit without starting first so tasks accumulate.
	low, _ := NewTask("ordered", map[string]string{}, WithPriority(1))
	mid, _ := NewTask("ordered", map[string]string{}, WithPriority(5))
	high, _ := NewTask("ordered", map[string]string{}, WithPriority(10))

	_ = q.Submit(ctx, low)
	_ = q.Submit(ctx, mid)
	_ = q.Submit(ctx, high)

	q.Start()
	defer q.Stop(ctx)

	var ids []string
	timeout := time.After(500 * time.Millisecond)
	for len(ids) < 3 {
		select {
		case id := <-order:
			ids = append(ids, id)
		case <-timeout:
			t.Fatalf("timed out waiting for tasks")
		}
	}

	if ids[0] != high.ID || ids[1] != mid.ID || ids[2] != low.ID {
		t.Fatalf("expected order high,mid,low, got %v", ids)
	}
}

func TestBoltStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")

	ctx := context.Background()
	store, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("NewBoltStore failed: %v", err)
	}

	task, err := NewTask("persist", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := store.Put(ctx, task); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen and verify recovery.
	store2, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer store2.Close()

	pending, err := store2.GetPending(ctx, 10)
	if err != nil {
		t.Fatalf("GetPending failed: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending task after reopen, got %d", len(pending))
	}
}

func TestQueue_Stats(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	q := NewQueue(store, 1)
	q.Register("ok", func(ctx context.Context, task *Task) error {
		return nil
	})
	q.Start()
	defer q.Stop(ctx)

	task, _ := NewTask("ok", map[string]string{})
	_ = q.Submit(ctx, task)

	for i := 0; i < 50; i++ {
		stats := q.Stats()
		if stats["completed"] == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("stats did not reflect completed task")
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
