package pluginloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"cipherlake/internal/events"
	"cipherlake/internal/logger"

	"go.uber.org/zap"
)

// ManifestEventSubscription declares a plugin's interest in internal events.
// The loader registers these on install and forwards matching events to the
// plugin's handler entry point (default "on_event").
type ManifestEventSubscription struct {
	Events  []string `json:"events,omitempty" yaml:"events,omitempty"`   // e.g. ["s3:ObjectCreated:*"]
	Handler string   `json:"handler,omitempty" yaml:"handler,omitempty"` // WASM entry function (default "on_event")
}

// pluginEventSubscription is a single runtime subscription held by a plugin.
type pluginEventSubscription struct {
	handle     uint64
	pluginName string
	pattern    string
	handler    string
}

// eventRegistry holds runtime event subscriptions for all plugins.
type eventRegistry struct {
	mu            sync.RWMutex
	subscriptions map[uint64]*pluginEventSubscription
	byPlugin      map[string]map[uint64]*pluginEventSubscription
	nextHandle    uint64
}

func newEventRegistry() *eventRegistry {
	return &eventRegistry{
		subscriptions: make(map[uint64]*pluginEventSubscription),
		byPlugin:      make(map[string]map[uint64]*pluginEventSubscription),
	}
}

// add registers a new subscription and returns its opaque handle.
func (r *eventRegistry) add(pluginName, pattern, handler string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextHandle++
	if r.nextHandle == 0 {
		r.nextHandle++
	}
	handle := r.nextHandle

	sub := &pluginEventSubscription{
		handle:     handle,
		pluginName: pluginName,
		pattern:    pattern,
		handler:    handler,
	}
	r.subscriptions[handle] = sub
	if r.byPlugin[pluginName] == nil {
		r.byPlugin[pluginName] = make(map[uint64]*pluginEventSubscription)
	}
	r.byPlugin[pluginName][handle] = sub
	return handle
}

// remove deletes a subscription by handle. Returns true if it existed.
func (r *eventRegistry) remove(handle uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	sub, ok := r.subscriptions[handle]
	if !ok {
		return false
	}
	delete(r.subscriptions, handle)
	if r.byPlugin[sub.pluginName] != nil {
		delete(r.byPlugin[sub.pluginName], handle)
		if len(r.byPlugin[sub.pluginName]) == 0 {
			delete(r.byPlugin, sub.pluginName)
		}
	}
	return true
}

// clear removes all subscriptions for a plugin.
func (r *eventRegistry) clear(pluginName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for handle := range r.byPlugin[pluginName] {
		delete(r.subscriptions, handle)
	}
	delete(r.byPlugin, pluginName)
}

// matches returns all subscriptions whose pattern matches the event type.
func (r *eventRegistry) matches(eventType string) []*pluginEventSubscription {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*pluginEventSubscription
	for _, sub := range r.subscriptions {
		if matchEventPattern(sub.pattern, eventType) {
			out = append(out, sub)
		}
	}
	return out
}

// matchEventPattern matches an event type pattern against an event type.
// Supports exact match and trailing wildcard: "s3:ObjectCreated:*".
func matchEventPattern(pattern, eventType string) bool {
	if pattern == eventType {
		return true
	}
	if strings.HasSuffix(pattern, ":*") {
		prefix := pattern[:len(pattern)-1]
		return strings.HasPrefix(eventType, prefix)
	}
	return false
}

// eventInvocation is the per-on_event call context.
type eventInvocation struct {
	pluginName string
	event      *events.Event
	payload    []byte
}

// SetEventBus wires the loader to the system event bus. The loader
// subscribes to all events and forwards matching ones to plugin on_event
// handlers. Call before any plugin subscribes to events.
func (l *Loader) SetEventBus(bus *events.EventBus) {
	l.mu.Lock()
	if l.eventBus == bus {
		l.mu.Unlock()
		return
	}

	oldStopCh := l.eventStopCh
	l.eventBus = bus
	l.eventStopCh = nil
	l.mu.Unlock()

	// Stop existing dispatch workers. We must release l.mu first so any
	// workers blocked on l.mu.RLock can finish and exit.
	if oldStopCh != nil {
		close(oldStopCh)
		l.eventDispatchWg.Wait()
	}

	if bus == nil {
		return
	}

	// Start dispatch workers that pull from the internal event channel.
	stopCh := make(chan struct{})
	l.mu.Lock()
	l.eventStopCh = stopCh
	l.mu.Unlock()
	l.eventDispatchWg.Add(l.eventWorkerCount)
	for i := 0; i < l.eventWorkerCount; i++ {
		go l.eventDispatchLoop(stopCh)
	}

	// Subscribe a wildcard callback. The callback is non-blocking so the
	// EventBus dispatch goroutine never stalls on a slow plugin.
	bus.SubscribeCallback("*", func(ev *events.Event) {
		select {
		case l.eventCh <- ev:
		default:
			// Internal event channel full; drop and log.
			zap.L().Warn("plugin loader event channel full, dropping event",
				zap.String("event_type", ev.EventType),
				zap.String("bucket", ev.Bucket),
				zap.String("key", ev.Key))
		}
	})
}

// eventDispatchLoop reads events from the internal channel and dispatches
// them to matching plugin subscriptions.
func (l *Loader) eventDispatchLoop(stopCh <-chan struct{}) {
	defer l.eventDispatchWg.Done()
	for {
		select {
		case <-stopCh:
			return
		case ev := <-l.eventCh:
			l.dispatchEventToPlugins(context.Background(), ev)
		}
	}
}

// dispatchEventToPlugins invokes on_event for every subscription that
// matches the event type. Errors are logged but do not block other plugins.
func (l *Loader) dispatchEventToPlugins(ctx context.Context, ev *events.Event) {
	if ev == nil {
		return
	}

	subs := l.events.matches(ev.EventType)
	if len(subs) == 0 {
		return
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		zap.L().Error("failed to marshal event for plugin dispatch", zap.Error(err))
		return
	}

	for _, sub := range subs {
		l.invokePluginEvent(ctx, sub, ev, payload)
	}
}

// invokePluginEvent borrows an instance and calls the plugin's event handler.
func (l *Loader) invokePluginEvent(ctx context.Context, sub *pluginEventSubscription, ev *events.Event, payload []byte) {
	l.mu.RLock()
	holder, ok := l.plugins[sub.pluginName]
	l.mu.RUnlock()
	if !ok || holder == nil || holder.pool == nil {
		return
	}

	inv := &eventInvocation{
		pluginName: sub.pluginName,
		event:      ev,
		payload:    payload,
	}
	handle := l.allocateEventContext(inv)
	defer l.releaseEventContext(handle)

	inst, err := holder.pool.Borrow(ctx)
	if err != nil {
		zap.L().Warn("failed to borrow instance for plugin event",
			zap.String("plugin", sub.pluginName),
			zap.String("event_type", ev.EventType),
			zap.Error(err))
		return
	}
	defer holder.pool.Return(inst)

	handler := sub.handler
	if handler == "" {
		handler = "on_event"
	}

	logger.AuditLogger("plugin.event.dispatch", sub.pluginName, "", "allowed", map[string]interface{}{
		"event_type": ev.EventType,
		"bucket":     ev.Bucket,
		"key":        ev.Key,
		"handler":    handler,
	})

	if _, err := inst.CallEntry(ctx, handler, handle); err != nil {
		zap.L().Warn("plugin on_event failed",
			zap.String("plugin", sub.pluginName),
			zap.String("handler", handler),
			zap.String("event_type", ev.EventType),
			zap.Error(err))
	}
}

// SubscribePluginEvent registers a runtime event subscription for a plugin.
// Returns the opaque subscription handle.
func (l *Loader) SubscribePluginEvent(pluginName, pattern, handler string) (uint64, error) {
	if pattern == "" {
		return 0, errors.New("event pattern is required")
	}
	if handler == "" {
		handler = "on_event"
	}
	handle := l.events.add(pluginName, pattern, handler)
	zap.L().Debug("plugin subscribed to event",
		zap.String("plugin", pluginName),
		zap.String("pattern", pattern),
		zap.Uint64("handle", handle))
	return handle, nil
}

// UnsubscribePluginEvent removes a runtime event subscription.
func (l *Loader) UnsubscribePluginEvent(pluginName string, handle uint64) error {
	sub, ok := l.events.lookup(handle)
	if !ok || sub.pluginName != pluginName {
		return fmt.Errorf("subscription %d not found for plugin %q", handle, pluginName)
	}
	l.events.remove(handle)
	return nil
}

// registerEventSubscriptionsFromManifest registers subscriptions declared in
// the plugin manifest.
func (l *Loader) registerEventSubscriptionsFromManifest(manifest *Manifest) {
	for _, sub := range manifest.EventSubscriptions {
		handler := sub.Handler
		if handler == "" {
			handler = "on_event"
		}
		for _, pattern := range sub.Events {
			if pattern == "" {
				continue
			}
			l.events.add(manifest.Name, pattern, handler)
		}
	}
}

// unregisterPluginEventSubscriptions removes all event subscriptions for a plugin.
func (l *Loader) unregisterPluginEventSubscriptions(pluginName string) {
	l.events.clear(pluginName)
}

// --- event handle management ---

func (l *Loader) allocateEventContext(inv *eventInvocation) uint64 {
	l.eventMu.Lock()
	defer l.eventMu.Unlock()
	l.nextEventHandle++
	if l.nextEventHandle == 0 {
		l.nextEventHandle++
	}
	h := l.nextEventHandle
	l.eventContexts[h] = inv
	return h
}

// GetEventContext returns the event invocation for the given handle.
func (l *Loader) GetEventContext(handle uint64) *eventInvocation {
	l.eventMu.Lock()
	defer l.eventMu.Unlock()
	return l.eventContexts[handle]
}

func (l *Loader) releaseEventContext(handle uint64) {
	l.eventMu.Lock()
	defer l.eventMu.Unlock()
	delete(l.eventContexts, handle)
}

// lookup returns a subscription by handle.
func (r *eventRegistry) lookup(handle uint64) (*pluginEventSubscription, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sub, ok := r.subscriptions[handle]
	return sub, ok
}

// EventBusPublish publishes an event to the system event bus on behalf of a
// plugin. Returns an error if no event bus is wired.
func (l *Loader) EventBusPublish(ctx context.Context, ev *events.Event) error {
	l.mu.RLock()
	bus := l.eventBus
	l.mu.RUnlock()
	if bus == nil {
		return errors.New("event bus not configured")
	}
	bus.Publish(ctx, ev)
	return nil
}

// PublishPluginEvent is a convenience helper that builds an Event from raw
// fields and publishes it to the event bus.
func (l *Loader) PublishPluginEvent(ctx context.Context, eventType, bucket, key string, payload []byte) error {
	ev := &events.Event{
		EventID:   fmt.Sprintf("plugin-%d", time.Now().UnixNano()),
		EventType: eventType,
		Bucket:    bucket,
		Key:       key,
		Size:      int64(len(payload)),
		Timestamp: time.Now(),
	}
	return l.EventBusPublish(ctx, ev)
}
