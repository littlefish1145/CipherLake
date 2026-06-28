package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status represents the lifecycle state of a queued task.
type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusDeadLetter Status = "dead_letter"
)

// Task is a unit of background work. It is serialized to the store and
// executed by a worker.
type Task struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Payload     []byte    `json:"payload"`
	Priority    int       `json:"priority"`
	Status      Status    `json:"status"`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Deadline    time.Time `json:"deadline,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// Validate returns an error if the task is malformed.
func (t *Task) Validate() error {
	if t.ID == "" {
		return errors.New("task id is required")
	}
	if t.Kind == "" {
		return errors.New("task kind is required")
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 1
	}
	return nil
}

// IsExpired reports whether the task has passed its deadline.
func (t *Task) IsExpired() bool {
	return !t.Deadline.IsZero() && time.Now().After(t.Deadline)
}

// Handler processes a single task. A non-nil error marks the task for retry
// (or dead-letter if attempts are exhausted).
type Handler func(ctx context.Context, task *Task) error

// TaskOption configures a newly created task.
type TaskOption func(*Task)

// NewTask creates a task with the given kind and JSON-serializable payload.
func NewTask(kind string, payload any, opts ...TaskOption) (*Task, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal task payload: %w", err)
	}

	now := time.Now()
	t := &Task{
		ID:          uuid.New().String(),
		Kind:        kind,
		Payload:     data,
		Priority:    0,
		Status:      StatusPending,
		Attempts:    0,
		MaxAttempts: 3,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	for _, opt := range opts {
		opt(t)
	}

	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// WithRetry sets the maximum number of execution attempts.
func WithRetry(n int) TaskOption {
	return func(t *Task) {
		if n > 0 {
			t.MaxAttempts = n
		}
	}
}

// WithDeadline sets an absolute deadline by which the task must complete.
func WithDeadline(deadline time.Time) TaskOption {
	return func(t *Task) {
		t.Deadline = deadline
	}
}

// WithDeadlineAfter sets a relative deadline from the moment the task is created.
func WithDeadlineAfter(d time.Duration) TaskOption {
	return func(t *Task) {
		t.Deadline = time.Now().Add(d)
	}
}

// WithPriority sets the task priority. Higher values are processed first.
func WithPriority(p int) TaskOption {
	return func(t *Task) {
		t.Priority = p
	}
}

// ParsePayload unmarshals the task payload into v.
func (t *Task) ParsePayload(v any) error {
	return json.Unmarshal(t.Payload, v)
}
