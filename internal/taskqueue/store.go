package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrTaskNotFound = errors.New("task not found")
	ErrStoreClosed  = errors.New("task store is closed")
)

// Store persists tasks and supports recovery after a crash.
type Store interface {
	// Put inserts a new task.
	Put(ctx context.Context, task *Task) error
	// GetPending returns pending tasks ordered by priority and creation time.
	GetPending(ctx context.Context, limit int) ([]*Task, error)
	// ClaimPending atomically marks pending tasks as processing and returns them.
	ClaimPending(ctx context.Context, limit int) ([]*Task, error)
	// RecoverProcessing returns tasks left processing by a previous process to pending.
	RecoverProcessing(ctx context.Context) error
	// GetByID returns a task by id.
	GetByID(ctx context.Context, id string) (*Task, error)
	// Update atomically updates an existing task.
	Update(ctx context.Context, task *Task) error
	// Delete removes a completed task from the queue.
	Delete(ctx context.Context, id string) error
	// ListDeadLetter returns tasks moved to the dead-letter queue.
	ListDeadLetter(ctx context.Context, limit int) ([]*Task, error)
	// Close releases resources held by the store.
	Close() error
}

// MemoryStore is an in-memory Store implementation intended for tests and
// single-node deployments that do not require durability.
type MemoryStore struct {
	mu     sync.RWMutex
	tasks  map[string]*Task
	closed atomic.Bool
}

// NewMemoryStore creates an in-memory task store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tasks: make(map[string]*Task),
	}
}

func (s *MemoryStore) checkOpen() error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	return nil
}

func (s *MemoryStore) Put(ctx context.Context, task *Task) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := cloneTask(task)
	stored.UpdatedAt = time.Now()
	s.tasks[task.ID] = stored
	return nil
}

func (s *MemoryStore) GetPending(ctx context.Context, limit int) ([]*Task, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pending []*Task
	for _, t := range s.tasks {
		if t.Status == StatusPending {
			pending = append(pending, cloneTask(t))
		}
	}
	pending = sortTasks(pending)
	if limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

func (s *MemoryStore) ClaimPending(ctx context.Context, limit int) ([]*Task, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var pending []*Task
	for _, task := range s.tasks {
		if task.Status == StatusPending {
			pending = append(pending, cloneTask(task))
		}
	}
	pending = sortTasks(pending)
	if limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}

	now := time.Now()
	for _, task := range pending {
		stored := s.tasks[task.ID]
		stored.Status = StatusProcessing
		stored.Attempts++
		stored.UpdatedAt = now
		*task = *cloneTask(stored)
	}
	return pending, nil
}

func (s *MemoryStore) RecoverProcessing(ctx context.Context) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, task := range s.tasks {
		if task.Status == StatusProcessing {
			task.Status = StatusPending
			task.UpdatedAt = now
		}
	}
	return nil
}

func (s *MemoryStore) GetByID(ctx context.Context, id string) (*Task, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return cloneTask(t), nil
}

func (s *MemoryStore) Update(ctx context.Context, task *Task) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[task.ID]; !ok {
		return ErrTaskNotFound
	}
	stored := cloneTask(task)
	stored.UpdatedAt = time.Now()
	s.tasks[task.ID] = stored
	return nil
}

func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
	return nil
}

func (s *MemoryStore) ListDeadLetter(ctx context.Context, limit int) ([]*Task, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var dead []*Task
	for _, t := range s.tasks {
		if t.Status == StatusDeadLetter {
			dead = append(dead, cloneTask(t))
		}
	}
	dead = sortTasks(dead)
	if limit > 0 && len(dead) > limit {
		dead = dead[:limit]
	}
	return dead, nil
}

func (s *MemoryStore) Close() error {
	s.closed.Store(true)
	return nil
}

func cloneTask(t *Task) *Task {
	data, _ := json.Marshal(t)
	var copy Task
	_ = json.Unmarshal(data, &copy)
	return &copy
}

func sortTasks(tasks []*Task) []*Task {
	result := make([]*Task, len(tasks))
	copy(result, tasks)
	// Sort by priority descending, then creation time ascending.
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i].Priority < result[j].Priority ||
				(result[i].Priority == result[j].Priority && result[i].CreatedAt.After(result[j].CreatedAt)) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}
