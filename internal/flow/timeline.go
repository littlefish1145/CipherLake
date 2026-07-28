package flow

import (
	"sort"
	"sync"
	"time"
)

type Timeline struct {
	mu      sync.RWMutex
	events  map[string][]TimelineEvent
	maxSize int
}

type TimelineEvent struct {
	Time     time.Time         `json:"time"`
	Type     EventType         `json:"type"`
	Step     string            `json:"step,omitempty"`
	Duration time.Duration     `json:"duration,omitempty"`
	Error    string            `json:"error,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type TimelineEntry struct {
	ExecutionID string          `json:"execution_id"`
	Workflow    string          `json:"workflow"`
	Events      []TimelineEvent `json:"events"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Status      ExecutionStatus `json:"status"`
}

func NewTimeline(maxEvents int) *Timeline {
	if maxEvents <= 0 {
		maxEvents = 1000
	}
	return &Timeline{
		events:  make(map[string][]TimelineEvent),
		maxSize: maxEvents,
	}
}

func (tl *Timeline) Record(executionID string, event Event) {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	entry := TimelineEvent{
		Time:     event.Time,
		Type:     event.Type,
		Step:     event.Step,
		Duration: event.Duration,
		Error:    event.Error,
		Metadata: event.Metadata,
	}

	tl.events[executionID] = append(tl.events[executionID], entry)

	if len(tl.events[executionID]) > tl.maxSize {
		tl.events[executionID] = tl.events[executionID][len(tl.events[executionID])-tl.maxSize:]
	}
}

func (tl *Timeline) Get(executionID string) []TimelineEvent {
	tl.mu.RLock()
	defer tl.mu.RUnlock()
	events := tl.events[executionID]
	result := make([]TimelineEvent, len(events))
	copy(result, events)
	sort.Slice(result, func(i, j int) bool { return result[i].Time.Before(result[j].Time) })
	return result
}

func (tl *Timeline) List(limit int) []string {
	tl.mu.RLock()
	defer tl.mu.RUnlock()
	var ids []string
	for id := range tl.events {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		ei := tl.events[ids[i]]
		ej := tl.events[ids[j]]
		if len(ei) == 0 || len(ej) == 0 {
			return len(ei) > 0
		}
		return ei[len(ei)-1].Time.After(ej[len(ej)-1].Time)
	})
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

func (tl *Timeline) BuildEntry(executor *Executor, executionID string) *TimelineEntry {
	exec, ok := executor.GetExecution(executionID)
	if !ok {
		return nil
	}

	events := tl.Get(executionID)
	return &TimelineEntry{
		ExecutionID: executionID,
		Workflow:    exec.Workflow,
		Events:      events,
		StartedAt:   exec.StartedAt,
		CompletedAt: exec.CompletedAt,
		Status:      exec.Status,
	}
}

func newTimelineSubscriber(tl *Timeline) EventSubscriber {
	return func(e Event) {
		if e.Execution != "" {
			tl.Record(e.Execution, e)
		}
	}
}
