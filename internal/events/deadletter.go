package events

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// DeadLetterEntry represents a failed event delivery stored in the dead letter queue.
type DeadLetterEntry struct {
	ID          string      `json:"id"`
	Event       *Event      `json:"event"`
	Rule        *NotificationRule `json:"rule"`
	Attempts    int         `json:"attempts"`
	LastAttempt time.Time   `json:"last_attempt"`
	LastError   string      `json:"last_error"`
	CreatedAt   time.Time   `json:"created_at"`
}

// DeadLetterQueue manages failed event deliveries with retry support.
type DeadLetterQueue struct {
	dir         string
	maxRetries  int
	retryBaseMS int
	metrics     *Metrics
	retryCh     chan *DeadLetterEntry
	stopCh      chan struct{}
}

// NewDeadLetterQueue creates a new dead letter queue.
func NewDeadLetterQueue(dir string, maxRetries, retryBaseMS int, metrics *Metrics) *DeadLetterQueue {
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if retryBaseMS <= 0 {
		retryBaseMS = 1000
	}
	return &DeadLetterQueue{
		dir:         dir,
		maxRetries:  maxRetries,
		retryBaseMS: retryBaseMS,
		metrics:     metrics,
		retryCh:     make(chan *DeadLetterEntry, 1000),
		stopCh:      make(chan struct{}),
	}
}

// Start begins the background retry processor.
func (q *DeadLetterQueue) Start(sender *WebhookSender) {
	go q.retryLoop(sender)
}

// Stop signals the background retry processor to stop.
func (q *DeadLetterQueue) Stop() {
	close(q.stopCh)
}

// Enqueue adds a failed delivery to the dead letter queue.
// If the event hasn't exceeded max retries, it will be retried with exponential backoff.
// Otherwise, it's written to the dead letter directory as a JSON file.
func (q *DeadLetterQueue) Enqueue(event *Event, rule *NotificationRule, attempts int, lastError string) {
	entry := &DeadLetterEntry{
		ID:          uuid.New().String(),
		Event:       event,
		Rule:        rule,
		Attempts:    attempts,
		LastAttempt: time.Now(),
		LastError:   lastError,
		CreatedAt:   time.Now(),
	}

	if attempts < q.maxRetries {
		// Schedule for retry
		select {
		case q.retryCh <- entry:
		default:
			// If retry channel is full, write to disk
			q.writeToDisk(entry)
			q.metrics.IncDeadLetter()
		}
	} else {
		// Max retries exceeded, write to dead letter directory
		q.writeToDisk(entry)
		q.metrics.IncDeadLetter()
	}
}

// RetryChannel returns the channel that receives entries scheduled for retry.
func (q *DeadLetterQueue) RetryChannel() <-chan *DeadLetterEntry {
	return q.retryCh
}

// retryLoop processes retries with exponential backoff.
func (q *DeadLetterQueue) retryLoop(sender *WebhookSender) {
	for {
		select {
		case <-q.stopCh:
			return
		case entry := <-q.retryCh:
			// Calculate exponential backoff: base * 2^attempt
			backoff := time.Duration(q.retryBaseMS) * time.Millisecond
			for i := 0; i < entry.Attempts; i++ {
				backoff *= 2
			}
			// Cap at 16 seconds
			if backoff > 16*time.Second {
				backoff = 16 * time.Second
			}

			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
				// Proceed with retry
			case <-q.stopCh:
				timer.Stop()
				return
			}

			result := sender.Send(entry.Event, entry.Rule.Destination)
			if result.Success {
				q.metrics.IncDeliverySuccess()
			} else {
				newAttempts := entry.Attempts + 1
				errMsg := "delivery failed"
				if result.Error != nil {
					errMsg = result.Error.Error()
				}
				if result.StatusCode > 0 {
					errMsg = fmt.Sprintf("HTTP %d: %s", result.StatusCode, errMsg)
				}
				q.metrics.IncDeliveryRetry()
				q.Enqueue(entry.Event, entry.Rule, newAttempts, errMsg)
			}
		}
	}
}

// writeToDisk persists a dead letter entry as a JSON file.
func (q *DeadLetterQueue) writeToDisk(entry *DeadLetterEntry) {
	if err := os.MkdirAll(q.dir, 0755); err != nil {
		log.Printf("[nexus] DLQ: failed to create directory %s: %v", q.dir, err)
		return
	}

	filename := fmt.Sprintf("%s_%s.json", entry.CreatedAt.Format("20060102-150405"), entry.ID[:8])
	path := filepath.Join(q.dir, filename)

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		log.Printf("[nexus] DLQ: failed to marshal entry %s: %v", entry.ID, err)
		return
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("[nexus] DLQ: failed to write entry to %s: %v", path, err)
	}
}
