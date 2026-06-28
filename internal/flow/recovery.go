package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type EventRecord struct {
	ID        string    `json:"id"`
	Type      EventType `json:"type"`
	Time      time.Time `json:"time"`
	Workflow  string    `json:"workflow,omitempty"`
	Step      string    `json:"step,omitempty"`
	Execution string    `json:"execution,omitempty"`
	Error     string    `json:"error,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type RecoveryStore interface {
	Append(record EventRecord) error
	Replay(since time.Time) ([]EventRecord, error)
	ReplayWorkflow(workflow string) ([]EventRecord, error)
	ReplayFailed() ([]EventRecord, error)
	Close() error
}

type fileRecoveryStore struct {
	mu       sync.Mutex
	dir      string
	file     *os.File
	encoder  *json.Encoder
}

func NewFileRecoveryStore(dir string) (RecoveryStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create recovery dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("recovery_%d.flow", time.Now().UnixNano()))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open recovery log: %w", err)
	}
	return &fileRecoveryStore{
		dir:     dir,
		file:    f,
		encoder: json.NewEncoder(f),
	}, nil
}

func (s *fileRecoveryStore) Append(record EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoder.Encode(record)
}

func (s *fileRecoveryStore) Replay(since time.Time) ([]EventRecord, error) {
	records, err := s.readAll()
	if err != nil {
		return nil, err
	}
	var filtered []EventRecord
	for _, r := range records {
		if r.Time.After(since) || r.Time.Equal(since) {
			filtered = append(filtered, r)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Time.Before(filtered[j].Time) })
	return filtered, nil
}

func (s *fileRecoveryStore) ReplayWorkflow(workflow string) ([]EventRecord, error) {
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	var filtered []EventRecord
	for _, r := range all {
		if r.Workflow == workflow {
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
}

func (s *fileRecoveryStore) ReplayFailed() ([]EventRecord, error) {
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	var filtered []EventRecord
	for _, r := range all {
		if r.Type == EventWorkflowFailed || r.Type == EventStepFailed {
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
}

func (s *fileRecoveryStore) readAll() ([]EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.file.Close()

	files, err := filepath.Glob(filepath.Join(s.dir, "recovery_*.flow"))
	if err != nil {
		return nil, err
	}

	var records []EventRecord
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		for dec.More() {
			var rec EventRecord
			if err := dec.Decode(&rec); err != nil {
				break
			}
			records = append(records, rec)
		}
	}

	f, err := os.OpenFile(s.file.Name(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		s.file = f
		s.encoder = json.NewEncoder(f)
	}

	return records, nil
}

func (s *fileRecoveryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

type recoverySubscriber struct {
	store RecoveryStore
}

func newRecoverySubscriber(store RecoveryStore) EventSubscriber {
	return func(e Event) {
		rec := EventRecord{
			ID:        e.ID,
			Type:      e.Type,
			Time:      e.Time,
			Workflow:  e.Workflow,
			Step:      e.Step,
			Execution: e.Execution,
			Error:     e.Error,
			Metadata:  e.Metadata,
		}
		store.Append(rec)
	}
}

type Replayer struct {
	store RecoveryStore
}

func NewReplayer(store RecoveryStore) *Replayer {
	return &Replayer{store: store}
}

func (r *Replayer) Replay(ctx context.Context, executor *Executor, since time.Time) error {
	records, err := r.store.Replay(since)
	if err != nil {
		return err
	}

	if len(records) == 0 {
		return nil
	}

	executor.telemetry.EventBus.Publish(Event{
		ID:   newID(),
		Type: EventRecoveryStarted,
		Time: time.Now(),
		Metadata: map[string]string{
			"since":    since.Format(time.RFC3339),
			"count":    fmt.Sprintf("%d", len(records)),
		},
	})

	failedExecutions := make(map[string]bool)
	for _, r := range records {
		if r.Type == EventWorkflowFailed || r.Type == EventWorkflowCompleted {
			failedExecutions[r.Execution] = r.Type == EventWorkflowFailed
		}
	}

	var replayed int
	for execID, isFailed := range failedExecutions {
		if !isFailed {
			continue
		}
		execRecs := filterRecords(records, execID)
		workflowName := ""
		wfKey := ""
		wfBucket := ""
		for _, r := range execRecs {
			if r.Workflow != "" {
				workflowName = r.Workflow
			}
		}

		exec, ok := executor.GetExecution(execID)
		if !ok || exec == nil {
			continue
		}
		wfKey = exec.ObjectKey
		wfBucket = exec.Bucket

		if workflowName == "" || wfKey == "" {
			continue
		}

		input := &ObjectInput{
			Key:    wfKey,
			Bucket: wfBucket,
		}
		if _, err := executor.Execute(ctx, workflowName, input); err != nil {
			executor.telemetry.EventBus.Publish(Event{
				ID:   newID(),
				Type: EventRecoveryFailed,
				Time: time.Now(),
				Error: err.Error(),
				Metadata: map[string]string{
					"execution": execID,
					"workflow":  workflowName,
				},
			})
			continue
		}
		replayed++
	}

	executor.telemetry.EventBus.Publish(Event{
		ID:   newID(),
		Type: EventRecoveryComplete,
		Time: time.Now(),
		Metadata: map[string]string{
			"replayed": fmt.Sprintf("%d", replayed),
			"total":    fmt.Sprintf("%d", len(records)),
		},
	})

	return nil
}

func filterRecords(records []EventRecord, execID string) []EventRecord {
	var filtered []EventRecord
	for _, r := range records {
		if r.Execution == execID {
			filtered = append(filtered, r)
		}
	}
	return filtered
}
