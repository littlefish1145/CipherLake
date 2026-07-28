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

func TestMemoryStoreDoesNotExposeTaskPointers(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	task, err := NewTask("test", map[string]string{})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := store.Put(ctx, task); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	task.Status = StatusDeadLetter
	pending, err := store.GetPending(ctx, 1)
	if err != nil {
		t.Fatalf("GetPending failed: %v", err)
	}
	if len(pending) != 1 || pending[0].Status != StatusPending {
		t.Fatalf("stored task changed after caller mutation: %+v", pending)
	}

	pending[0].Status = StatusProcessing
	fetched, err := store.GetByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if fetched.Status != StatusPending {
		t.Fatalf("stored task changed after read mutation: %s", fetched.Status)
	}
}

func TestMemoryStoreClaimPendingAndRecoverProcessing(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	task, err := NewTask("test", map[string]string{})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := store.Put(ctx, task); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	claimed, err := store.ClaimPending(ctx, 1)
	if err != nil {
		t.Fatalf("ClaimPending failed: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Status != StatusProcessing || claimed[0].Attempts != 1 {
		t.Fatalf("claimed task = %+v, want processing task with one attempt", claimed)
	}

	claimed, err = store.ClaimPending(ctx, 1)
	if err != nil {
		t.Fatalf("second ClaimPending failed: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("second claim = %+v, want no tasks", claimed)
	}

	if err := store.RecoverProcessing(ctx); err != nil {
		t.Fatalf("RecoverProcessing failed: %v", err)
	}
	pending, err := store.GetPending(ctx, 1)
	if err != nil {
		t.Fatalf("GetPending failed: %v", err)
	}
	if len(pending) != 1 || pending[0].Status != StatusPending || pending[0].Attempts != 1 {
		t.Fatalf("recovered task = %+v, want pending task retaining attempts", pending)
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

func TestQueueStopInterruptsRetryBackoff(t *testing.T) {
	store := NewMemoryStore()
	defer store.Close()

	q := NewQueue(store, 1, WithPollInterval(time.Millisecond), WithRetryBase(time.Hour))
	started := make(chan struct{})
	q.Register("fail", func(context.Context, *Task) error {
		close(started)
		return errors.New("expected failure")
	})
	q.Start()

	task, err := NewTask("fail", map[string]string{}, WithRetry(2))
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := q.Submit(context.Background(), task); err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler was not called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := q.Stop(ctx); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

func TestQueueStopClosesStoreAfterWorkersExit(t *testing.T) {
	store := NewMemoryStore()
	q := NewQueue(store, 1)
	q.Start()

	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if _, err := store.GetPending(context.Background(), 1); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("store error after Stop = %v, want %v", err, ErrStoreClosed)
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

func TestQueueHandlerPanicMovesTaskToDeadLetter(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	defer store.Close()

	q := NewQueue(store, 1, WithPollInterval(time.Millisecond))
	q.Register("panic", func(context.Context, *Task) error {
		panic("handler failure")
	})
	q.Start()
	defer q.Stop(ctx)

	task, err := NewTask("panic", map[string]string{}, WithRetry(1))
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := q.Submit(ctx, task); err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	deadline := time.After(time.Second)
	for {
		dead, err := store.ListDeadLetter(ctx, 1)
		if err != nil {
			t.Fatalf("ListDeadLetter failed: %v", err)
		}
		if len(dead) == 1 {
			if dead[0].Error == "" {
				t.Fatal("dead-letter task is missing panic error")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("panicking task was not moved to dead letter")
		case <-time.After(time.Millisecond):
		}
	}
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

// TestBoltStoreClaimPendingAndRecoverProcessing mirrors the memory store test
// to verify Bolt and Memory implementations behave identically for the
// claim/retry lifecycle.
func TestBoltStoreClaimPendingAndRecoverProcessing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")

	store, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("NewBoltStore failed: %v", err)
	}
	defer store.Close()

	task, err := NewTask("test", map[string]string{})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := store.Put(ctx, task); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	claimed, err := store.ClaimPending(ctx, 1)
	if err != nil {
		t.Fatalf("ClaimPending failed: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Status != StatusProcessing || claimed[0].Attempts != 1 {
		t.Fatalf("claimed task = %+v, want processing task with one attempt", claimed)
	}

	claimed, err = store.ClaimPending(ctx, 1)
	if err != nil {
		t.Fatalf("second ClaimPending failed: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("second claim = %+v, want no tasks", claimed)
	}

	if err := store.RecoverProcessing(ctx); err != nil {
		t.Fatalf("RecoverProcessing failed: %v", err)
	}
	pending, err := store.GetPending(ctx, 1)
	if err != nil {
		t.Fatalf("GetPending failed: %v", err)
	}
	if len(pending) != 1 || pending[0].Status != StatusPending || pending[0].Attempts != 1 {
		t.Fatalf("recovered task = %+v, want pending task retaining attempts", pending)
	}
}

// TestBoltStoreRecoverProcessingAcrossRestart simulates a crash during task
// processing: tasks are claimed (status=processing) and the process restarts
// without completing them. On reopen + RecoverProcessing, the in-flight tasks
// should return to pending state with their attempt counts preserved.
func TestBoltStoreRecoverProcessingAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")

	store1, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("NewBoltStore failed: %v", err)
	}

	task1, err := NewTask("recover", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	task2, err := NewTask("recover", map[string]string{"k": "v2"})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := store1.Put(ctx, task1); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := store1.Put(ctx, task2); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	claimed, err := store1.ClaimPending(ctx, 2)
	if err != nil {
		t.Fatalf("ClaimPending failed: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected 2 claimed tasks, got %d", len(claimed))
	}
	for _, c := range claimed {
		if c.Status != StatusProcessing || c.Attempts != 1 {
			t.Fatalf("claimed task = %+v, want processing with 1 attempt", c)
		}
	}

	// Simulate crash: close without completing the tasks.
	if err := store1.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen — RecoverProcessing should move processing tasks back to pending.
	store2, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer store2.Close()

	if err := store2.RecoverProcessing(ctx); err != nil {
		t.Fatalf("RecoverProcessing failed: %v", err)
	}

	pending, err := store2.GetPending(ctx, 10)
	if err != nil {
		t.Fatalf("GetPending failed: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending tasks after recovery, got %d", len(pending))
	}
	for _, p := range pending {
		if p.Status != StatusPending {
			t.Fatalf("recovered task status = %s, want pending", p.Status)
		}
		if p.Attempts != 1 {
			t.Fatalf("recovered task attempts = %d, want 1 (preserved from before crash)", p.Attempts)
		}
	}
}

// TestBoltStoreDeadLetterPersistsAcrossRestart verifies that dead-letter tasks
// survive a restart and remain in the dead-letter state.
func TestBoltStoreDeadLetterPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")

	store1, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("NewBoltStore failed: %v", err)
	}

	task, err := NewTask("dead", map[string]string{}, WithRetry(0))
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := store1.Put(ctx, task); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	// Manually move to dead-letter state.
	task.Status = StatusDeadLetter
	task.Error = "permanently failed"
	if err := store1.Update(ctx, task); err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	store2, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer store2.Close()

	dead, err := store2.ListDeadLetter(ctx, 10)
	if err != nil {
		t.Fatalf("ListDeadLetter failed: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("expected 1 dead-letter task after restart, got %d", len(dead))
	}
	if dead[0].ID != task.ID {
		t.Fatalf("dead-letter task id = %s, want %s", dead[0].ID, task.ID)
	}
	if dead[0].Status != StatusDeadLetter {
		t.Fatalf("dead-letter task status = %s, want dead-letter", dead[0].Status)
	}
	if dead[0].Error != "permanently failed" {
		t.Fatalf("dead-letter task error = %q, want %q", dead[0].Error, "permanently failed")
	}

	// RecoverProcessing should NOT move dead-letter tasks back to pending.
	if err := store2.RecoverProcessing(ctx); err != nil {
		t.Fatalf("RecoverProcessing failed: %v", err)
	}
	dead, err = store2.ListDeadLetter(ctx, 10)
	if err != nil {
		t.Fatalf("ListDeadLetter after RecoverProcessing failed: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("RecoverProcessing should not affect dead-letter tasks; got %d dead", len(dead))
	}
}

// TestBoltStoreDoesNotExposeTaskPointers mirrors the memory store test to
// verify the Bolt implementation also returns independent copies.
func TestBoltStoreDoesNotExposeTaskPointers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")
	store, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("NewBoltStore failed: %v", err)
	}
	defer store.Close()

	task, err := NewTask("test", map[string]string{})
	if err != nil {
		t.Fatalf("NewTask failed: %v", err)
	}
	if err := store.Put(ctx, task); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	task.Status = StatusDeadLetter
	pending, err := store.GetPending(ctx, 1)
	if err != nil {
		t.Fatalf("GetPending failed: %v", err)
	}
	if len(pending) != 1 || pending[0].Status != StatusPending {
		t.Fatalf("stored task changed after caller mutation: %+v", pending)
	}

	pending[0].Status = StatusProcessing
	fetched, err := store.GetByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if fetched.Status != StatusPending {
		t.Fatalf("stored task changed after read mutation: %s", fetched.Status)
	}
}

// TestQueue_BoltRetryAndDeadLetter verifies the Queue's retry/dead-letter
// semantics work the same with the Bolt backing store as with Memory.
func TestQueue_BoltRetryAndDeadLetter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")
	store, err := NewBoltStore(path)
	if err != nil {
		t.Fatalf("NewBoltStore failed: %v", err)
	}
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

	time.Sleep(500 * time.Millisecond)

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
