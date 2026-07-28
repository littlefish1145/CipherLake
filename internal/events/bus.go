package events

import (
	"context"
	"hash/fnv"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Event represents a storage event in the CipherLake system.
type Event struct {
	EventID      string    `json:"event_id"`
	EventType    string    `json:"event_type"` // e.g., "s3:ObjectCreated:Put"
	Bucket       string    `json:"bucket"`
	Key          string    `json:"key"`
	VersionID    string    `json:"version_id"`
	ETag         string    `json:"etag"`
	Size         int64     `json:"size"`
	Timestamp    time.Time `json:"timestamp"`
	RequesterARN string    `json:"requester_arn"`
	SourceIP     string    `json:"source_ip"`
}

// subscription holds a bucket-level notification rule subscription.
type subscription struct {
	bucket string
	rule   *NotificationRule
}

// eventDelivery bundles an event with its matching rules for delivery.
type eventDelivery struct {
	event *Event
	rules []*NotificationRule
}

// eventWorker processes events sequentially, preserving order for same key.
type eventWorker struct {
	id        int
	ch        chan *eventDelivery
	sender    *WebhookSender
	dlq       *DeadLetterQueue
	metrics   *Metrics
	drainWg   *sync.WaitGroup
	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

func newEventWorker(id int, sender *WebhookSender, dlq *DeadLetterQueue, metrics *Metrics, drainWg *sync.WaitGroup) *eventWorker {
	return &eventWorker{
		id:      id,
		ch:      make(chan *eventDelivery, 256),
		sender:  sender,
		dlq:     dlq,
		metrics: metrics,
		drainWg: drainWg,
		stopCh:  make(chan struct{}),
	}
}

func (w *eventWorker) start() {
	w.startOnce.Do(func() {
		w.wg.Add(1)
		go w.processLoop()
	})
}

func (w *eventWorker) stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		w.wg.Wait()
	})
}

func (w *eventWorker) enqueue(delivery *eventDelivery) {
	select {
	case w.ch <- delivery:
	default:
		// Worker is overwhelmed - track the dropped event for monitoring
		w.metrics.IncDropped()
		// Decrement drainWg since this event will never be delivered
		w.drainWg.Done()
	}
}

func (w *eventWorker) processLoop() {
	defer w.wg.Done()
	for {
		select {
		case <-w.stopCh:
			return
		case delivery := <-w.ch:
			w.deliver(delivery)
		}
	}
}

func (w *eventWorker) deliver(delivery *eventDelivery) {
	defer w.drainWg.Done()

	event := delivery.event
	for _, rule := range delivery.rules {
		switch rule.Destination.Type {
		case "webhook":
			result := w.sender.Send(event, rule.Destination)
			if result.Success {
				w.metrics.IncDeliverySuccess()
			} else {
				errMsg := ""
				if result.Error != nil {
					errMsg = result.Error.Error()
				}
				w.metrics.IncDeliveryFailed()
				w.dlq.Enqueue(event, rule, 1, errMsg)
			}
		default:
			// Unsupported destination type; treat as failed delivery
			w.metrics.IncDeliveryFailed()
		}
	}
}

// callbackSubscription is a lightweight callback-based subscription used
// by internal consumers such as the plugin loader.
type callbackSubscription struct {
	bucket string
	fn     func(*Event)
}

// EventBus provides in-process publish/subscribe with key-based sharding
// for same-key ordering guarantees.
type EventBus struct {
	mu            sync.RWMutex
	subscriptions map[string][]*subscription // bucket -> list of subscriptions
	callbacks     []*callbackSubscription    // callback subscriptions (e.g. plugin loader)
	workers       []*eventWorker
	numWorkers    int
	sender        *WebhookSender
	dlq           *DeadLetterQueue
	metrics       *Metrics
	eventCh       chan *Event
	stopCh        chan struct{}
	drainWg       sync.WaitGroup // tracks in-flight events from Publish to delivery completion
	dispatchWg    sync.WaitGroup // tracks the dispatchLoop goroutine
	startOnce     sync.Once
	stopOnce      sync.Once
}

// NewEventBus creates a new EventBus with the given configuration.
func NewEventBus(numWorkers int, webhookTimeout time.Duration, dlqDir string, maxRetries, retryBaseMS int) *EventBus {
	if numWorkers <= 0 {
		numWorkers = 16
	}

	metrics := NewMetrics()
	sender := NewWebhookSender(webhookTimeout)
	dlq := NewDeadLetterQueue(dlqDir, maxRetries, retryBaseMS, metrics)

	bus := &EventBus{
		subscriptions: make(map[string][]*subscription),
		numWorkers:    numWorkers,
		sender:        sender,
		dlq:           dlq,
		metrics:       metrics,
		eventCh:       make(chan *Event, 4096),
		stopCh:        make(chan struct{}),
	}

	// Create worker goroutines for key-based sharding
	bus.workers = make([]*eventWorker, numWorkers)
	for i := 0; i < numWorkers; i++ {
		bus.workers[i] = newEventWorker(i, sender, dlq, metrics, &bus.drainWg)
	}

	return bus
}

// Start begins event processing workers and the DLQ retry loop.
// It is safe to call multiple times; subsequent calls are no-ops.
func (b *EventBus) Start() {
	b.startOnce.Do(func() {
		for _, w := range b.workers {
			w.start()
		}
		b.dlq.Start(b.sender)
		b.dispatchWg.Add(1)
		go b.dispatchLoop()
	})
}

// SetSSRFBypass enables or disables SSRF URL validation on the webhook sender.
// This should only be set to true for testing with local HTTP servers.
func (b *EventBus) SetSSRFBypass(bypass bool) {
	b.sender.ssrfBypass = bypass
}

// Stop gracefully shuts down the event bus.
// It is safe to call multiple times; subsequent calls are no-ops.
// Shutdown order: stop dispatch → drain in-flight → stop workers → stop DLQ.
// Workers are stopped before the DLQ so that no new retry entries can be
// enqueued after the DLQ retry loop has exited.
func (b *EventBus) Stop() {
	b.stopOnce.Do(func() {
		close(b.stopCh)

		// Wait for the dispatch loop to exit so no new events are routed.
		b.dispatchWg.Wait()

		// Wait for in-flight events to drain with a 30-second timeout.
		drained := make(chan struct{})
		go func() {
			b.drainWg.Wait()
			close(drained)
		}()

		select {
		case <-drained:
			// All in-flight events delivered successfully
		case <-time.After(30 * time.Second):
			log.Printf("[cipherlake] EventBus: drain timeout expired, some events may have been lost")
		}

		// Stop workers before stopping the DLQ so that no new entries are
		// enqueued after the DLQ retry loop has exited.
		for _, w := range b.workers {
			w.stop()
		}
		b.dlq.Stop()
	})
}

// Publish publishes an event asynchronously to all matching subscribers.
// If the bus is shutting down or the context is cancelled, the event is
// dropped and the in-flight counter is decremented.
func (b *EventBus) Publish(ctx context.Context, event *Event) {
	if event.EventID == "" {
		event.EventID = uuid.New().String()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	b.drainWg.Add(1)

	select {
	case b.eventCh <- event:
	case <-ctx.Done():
		b.drainWg.Done()
	case <-b.stopCh:
		b.drainWg.Done()
	}
}

// Subscribe registers a notification rule for a bucket.
func (b *EventBus) Subscribe(bucket string, rule *NotificationRule) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	sub := &subscription{
		bucket: bucket,
		rule:   rule,
	}
	b.subscriptions[bucket] = append(b.subscriptions[bucket], sub)
	return nil
}

// Unsubscribe removes a notification rule for a bucket by rule ID.
func (b *EventBus) Unsubscribe(bucket, ruleID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	subs, ok := b.subscriptions[bucket]
	if !ok {
		return nil
	}

	filtered := make([]*subscription, 0, len(subs))
	for _, s := range subs {
		if s.rule.ID != ruleID {
			filtered = append(filtered, s)
		}
	}
	b.subscriptions[bucket] = filtered
	return nil
}

// SubscribeCallback registers a synchronous callback for events matching
// bucket. bucket "*" matches events from any bucket. Callbacks are invoked
// in the dispatch goroutine; callers that need non-blocking delivery should
// hand the event off to their own goroutine/channel inside fn.
func (b *EventBus) SubscribeCallback(bucket string, fn func(*Event)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.callbacks = append(b.callbacks, &callbackSubscription{bucket: bucket, fn: fn})
}

// GetMetrics returns the metrics instance.
func (b *EventBus) GetMetrics() *Metrics {
	return b.metrics
}

// dispatchLoop reads events from the channel and dispatches them to the
// appropriate worker based on key hash (for same-key ordering).
// After receiving the stop signal, it drains any remaining events from the
// channel before exiting so that in-flight publishes are not silently dropped.
func (b *EventBus) dispatchLoop() {
	defer b.dispatchWg.Done()
	for {
		select {
		case <-b.stopCh:
			b.drainEventChannel()
			return
		case event := <-b.eventCh:
			b.dispatch(event)
		}
	}
}

// drainEventChannel dispatches any events remaining in eventCh after the bus
// has been signalled to stop. Publish checks stopCh before sending, so no new
// events can arrive once draining begins.
func (b *EventBus) drainEventChannel() {
	for {
		select {
		case event := <-b.eventCh:
			b.dispatch(event)
		default:
			return
		}
	}
}

// dispatch routes a single event to callbacks and then to the target worker,
// or decrements the drain counter if no matching rules exist.
func (b *EventBus) dispatch(event *Event) {
	b.invokeCallbacks(event)

	rules := b.getMatchingRules(event)
	if len(rules) == 0 {
		// No matching rules, event will not be delivered - decrement drainWg
		b.drainWg.Done()
		return
	}

	delivery := &eventDelivery{
		event: event,
		rules: rules,
	}

	workerIdx := b.keyToWorker(event.Key)
	b.workers[workerIdx].enqueue(delivery)
}

// invokeCallbacks calls all registered callback subscriptions whose bucket
// matches the event. Callback failures are swallowed so they cannot break
// rule-based delivery.
func (b *EventBus) invokeCallbacks(event *Event) {
	b.mu.RLock()
	callbacks := make([]*callbackSubscription, len(b.callbacks))
	copy(callbacks, b.callbacks)
	b.mu.RUnlock()

	for _, cb := range callbacks {
		if cb.bucket != "*" && cb.bucket != event.Bucket {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[cipherlake] EventBus callback panic: %v", r)
				}
			}()
			cb.fn(event)
		}()
	}
}

// keyToWorker maps an event key to a worker index using FNV-1a hash.
func (b *EventBus) keyToWorker(key string) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32()) % b.numWorkers
}

// getMatchingRules returns all rules that match the given event's bucket and filters.
// Rules registered under bucket "*" match events from any bucket.
func (b *EventBus) getMatchingRules(event *Event) []*NotificationRule {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var rules []*NotificationRule
	for bucket, subs := range b.subscriptions {
		if bucket != "*" && bucket != event.Bucket {
			continue
		}
		for _, sub := range subs {
			if sub.rule.Matches(event) {
				rules = append(rules, sub.rule)
			}
		}
	}
	return rules
}
