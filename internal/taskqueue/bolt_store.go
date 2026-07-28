package taskqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

const taskBucket = "tasks"

// BoltStore persists tasks in a BoltDB database. It uses a separate database
// file from the metadata store to avoid contention and keep the package
// self-contained.
type BoltStore struct {
	mu sync.RWMutex
	db *bolt.DB
}

// NewBoltStore opens (or creates) a BoltDB task store at path.
func NewBoltStore(path string) (*BoltStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("failed to create task store directory: %w", err)
	}

	db, err := bolt.Open(path, 0666, &bolt.Options{
		Timeout:      5 * time.Second,
		FreelistType: bolt.FreelistMapType,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open task store: %w", err)
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(taskBucket))
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize task bucket: %w", err)
	}

	return &BoltStore{db: db}, nil
}

func (s *BoltStore) Put(ctx context.Context, task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		return b.Put([]byte(task.ID), data)
	})
}

func (s *BoltStore) GetPending(ctx context.Context, limit int) ([]*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pending []*Task
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		return b.ForEach(func(k, v []byte) error {
			var t Task
			if err := json.Unmarshal(v, &t); err != nil {
				zap.L().Warn("bolt task store: skipping corrupted record",
					zap.ByteString("key", k), zap.Error(err))
				return nil
			}
			if t.Status == StatusPending {
				pending = append(pending, &t)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	pending = sortTasks(pending)
	if limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

func (s *BoltStore) ClaimPending(ctx context.Context, limit int) ([]*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var pending []*Task
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if err := b.ForEach(func(k, v []byte) error {
			var task Task
			if err := json.Unmarshal(v, &task); err != nil {
				zap.L().Warn("bolt task store: skipping corrupted record during claim",
					zap.ByteString("key", k), zap.Error(err))
				return nil
			}
			if task.Status == StatusPending {
				pending = append(pending, &task)
			}
			return nil
		}); err != nil {
			return err
		}

		pending = sortTasks(pending)
		if limit > 0 && len(pending) > limit {
			pending = pending[:limit]
		}

		now := time.Now()
		for _, task := range pending {
			task.Status = StatusProcessing
			task.Attempts++
			task.UpdatedAt = now
			data, err := json.Marshal(task)
			if err != nil {
				return fmt.Errorf("failed to marshal claimed task: %w", err)
			}
			if err := b.Put([]byte(task.ID), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pending, nil
}

func (s *BoltStore) RecoverProcessing(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		var processing []*Task
		if err := b.ForEach(func(k, v []byte) error {
			var task Task
			if err := json.Unmarshal(v, &task); err != nil {
				zap.L().Warn("bolt task store: skipping corrupted record during recover",
					zap.ByteString("key", k), zap.Error(err))
				return nil
			}
			if task.Status == StatusProcessing {
				processing = append(processing, &task)
			}
			return nil
		}); err != nil {
			return err
		}

		now := time.Now()
		for _, task := range processing {
			task.Status = StatusPending
			task.UpdatedAt = now
			data, err := json.Marshal(task)
			if err != nil {
				return fmt.Errorf("failed to marshal recovered task: %w", err)
			}
			if err := b.Put([]byte(task.ID), data); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *BoltStore) GetByID(ctx context.Context, id string) (*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var task *Task
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		data := b.Get([]byte(id))
		if data == nil {
			return ErrTaskNotFound
		}
		return json.Unmarshal(data, &task)
	})
	return task, err
}

func (s *BoltStore) Update(ctx context.Context, task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b.Get([]byte(task.ID)) == nil {
			return ErrTaskNotFound
		}
		return b.Put([]byte(task.ID), data)
	})
}

func (s *BoltStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		return b.Delete([]byte(id))
	})
}

func (s *BoltStore) ListDeadLetter(ctx context.Context, limit int) ([]*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var dead []*Task
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		return b.ForEach(func(k, v []byte) error {
			var t Task
			if err := json.Unmarshal(v, &t); err != nil {
				zap.L().Warn("bolt task store: skipping corrupted record during dead-letter list",
					zap.ByteString("key", k), zap.Error(err))
				return nil
			}
			if t.Status == StatusDeadLetter {
				dead = append(dead, &t)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	dead = sortTasks(dead)
	if limit > 0 && len(dead) > limit {
		dead = dead[:limit]
	}
	return dead, nil
}

func (s *BoltStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}
