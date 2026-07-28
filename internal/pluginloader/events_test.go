package pluginloader

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"cipherlake/internal/events"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalEventPluginBytes is a hand-built WASM module that exports all
// mandatory entry points plus on_event (signature i64 -> i32).
var minimalEventPluginBytes = []byte{
	// WASM magic + version
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,

	// Type section (id=1, size=19)
	0x01, 0x13,
	0x03, // 3 types
	// t0: (i64) -> i32
	0x60, 0x01, 0x7e, 0x01, 0x7f,
	// t1: (i64, i64) -> i32
	0x60, 0x02, 0x7e, 0x7e, 0x01, 0x7f,
	// t2: (i64, i64, i64) -> i32
	0x60, 0x03, 0x7e, 0x7e, 0x7e, 0x01, 0x7f,

	// Function section (id=3, size=6)
	0x03, 0x06,
	0x05, 0x00, 0x01, 0x02, 0x01, 0x00,

	// Memory section (id=5, size=3)
	0x05, 0x03,
	0x01, 0x00, 0x01,

	// Export section (id=7, size=66)
	0x07, 0x42,
	0x06, // 6 exports
	// "memory"
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00,
	// "wasm_init"
	0x09, 0x77, 0x61, 0x73, 0x6d, 0x5f, 0x69, 0x6e, 0x69, 0x74, 0x00, 0x00,
	// "on_hook"
	0x07, 0x6f, 0x6e, 0x5f, 0x68, 0x6f, 0x6f, 0x6b, 0x00, 0x01,
	// "on_request"
	0x0a, 0x6f, 0x6e, 0x5f, 0x72, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x00, 0x02,
	// "on_task"
	0x07, 0x6f, 0x6e, 0x5f, 0x74, 0x61, 0x73, 0x6b, 0x00, 0x03,
	// "on_event"
	0x08, 0x6f, 0x6e, 0x5f, 0x65, 0x76, 0x65, 0x6e, 0x74, 0x00, 0x04,

	// Code section (id=10, size=26)
	0x0a, 0x1a,
	0x05, // 5 function bodies
	// body for wasm_init
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// body for on_hook
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// body for on_request
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// body for on_task
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// body for on_event
	0x04, 0x00, 0x41, 0x00, 0x0b,
}

func minimalEventManifest(name string) *Manifest {
	return &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               name,
		Version:            "0.1.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"event:subscribe"},
	}
}

func TestEventRegistry_AddRemoveClear(t *testing.T) {
	r := newEventRegistry()

	h1 := r.add("p1", "s3:*", "on_event")
	require.NotZero(t, h1)
	h2 := r.add("p1", "s3:ObjectCreated:*", "on_event")
	require.NotZero(t, h2)

	require.Len(t, r.subscriptions, 2)
	require.Len(t, r.byPlugin["p1"], 2)

	require.True(t, r.remove(h1))
	require.Len(t, r.subscriptions, 1)
	require.Len(t, r.byPlugin["p1"], 1)

	require.False(t, r.remove(9999))

	r.clear("p1")
	require.Empty(t, r.subscriptions)
	require.Empty(t, r.byPlugin)
}

func TestEventRegistry_Matches(t *testing.T) {
	r := newEventRegistry()
	r.add("p1", "s3:ObjectCreated:Put", "on_event")
	r.add("p2", "s3:ObjectCreated:*", "on_event")
	r.add("p3", "s3:*", "on_event")
	r.add("p4", "audit:*", "on_event")

	matches := r.matches("s3:ObjectCreated:Put")
	require.Len(t, matches, 3)
	got := make(map[string]bool)
	for _, m := range matches {
		got[m.pluginName] = true
	}
	require.True(t, got["p1"])
	require.True(t, got["p2"])
	require.True(t, got["p3"])
}

func TestMatchEventPattern(t *testing.T) {
	cases := []struct {
		pattern string
		event   string
		match   bool
	}{
		{"s3:ObjectCreated:Put", "s3:ObjectCreated:Put", true},
		{"s3:ObjectCreated:*", "s3:ObjectCreated:Put", true},
		{"s3:ObjectCreated:*", "s3:ObjectCreated:Copy", true},
		{"s3:*", "s3:ObjectCreated:Put", true},
		{"s3:*", "audit:Login", false},
		{"s3:ObjectCreated:Put", "s3:ObjectCreated:Copy", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.match, matchEventPattern(c.pattern, c.event), "pattern=%q event=%q", c.pattern, c.event)
	}
}

func TestLoader_SubscribeUnsubscribePluginEvent(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	handle, err := l.SubscribePluginEvent("p1", "s3:*", "")
	require.NoError(t, err)
	require.NotZero(t, handle)

	sub, ok := l.events.lookup(handle)
	require.True(t, ok)
	assert.Equal(t, "on_event", sub.handler)

	err = l.UnsubscribePluginEvent("p2", handle)
	require.Error(t, err)

	require.NoError(t, l.UnsubscribePluginEvent("p1", handle))
	_, ok = l.events.lookup(handle)
	require.False(t, ok)
}

func TestLoader_RegisterEventSubscriptionsFromManifest(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := minimalEventManifest("event-manifest-test")
	manifest.EventSubscriptions = []ManifestEventSubscription{
		{Events: []string{"s3:ObjectCreated:*", "audit:*"}, Handler: "custom_handler"},
		{Events: []string{""}}, // empty pattern should be skipped
	}

	l.registerEventSubscriptionsFromManifest(manifest)

	require.Len(t, l.events.byPlugin["event-manifest-test"], 2)
}

func TestLoader_EventContextLifecycle(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	inv := &eventInvocation{pluginName: "p1", payload: []byte("hello")}
	h := l.allocateEventContext(inv)
	require.NotZero(t, h)

	got := l.GetEventContext(h)
	require.Equal(t, inv, got)

	l.releaseEventContext(h)
	require.Nil(t, l.GetEventContext(h))
}

func TestLoader_DispatchEventToPlugins(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	manifest := minimalEventManifest("event-dispatch-test")
	manifest.EventSubscriptions = []ManifestEventSubscription{
		{Events: []string{"s3:ObjectCreated:*"}},
	}
	require.NoError(t, l.InstallPlugin(ctx, manifest, minimalEventPluginBytes, nil, ""))

	payload, err := json.Marshal(&events.Event{
		EventType: "s3:ObjectCreated:Put",
		Bucket:    "bkt",
		Key:       "key",
	})
	require.NoError(t, err)

	// Direct dispatch avoids EventBus drain timeout while exercising the
	// invokePluginEvent path.
	l.dispatchEventToPlugins(ctx, &events.Event{
		EventType: "s3:ObjectCreated:Put",
		Bucket:    "bkt",
		Key:       "key",
	})

	// If the plugin was invoked, no panic occurred. We can also verify the
	// event context lifecycle by allocating a context directly.
	inv := &eventInvocation{pluginName: "event-dispatch-test", payload: payload}
	h := l.allocateEventContext(inv)
	require.NotZero(t, h)
	l.releaseEventContext(h)
}

func TestLoader_SetEventBus_NoDispatchWithoutSubscription(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	bus := events.NewEventBus(2, 5*time.Second, t.TempDir(), 3, 100)
	defer bus.Stop()
	l.SetEventBus(bus)

	// No subscriptions; dispatch should return early.
	l.dispatchEventToPlugins(context.Background(), &events.Event{EventType: "s3:ObjectCreated:Put"})
}

func TestLoader_SetEventBus_NilBusStopsWorkers(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	bus := events.NewEventBus(2, 5*time.Second, t.TempDir(), 3, 100)
	defer bus.Stop()
	l.SetEventBus(bus)
	require.NotNil(t, l.eventBus)

	l.SetEventBus(nil)
	require.Nil(t, l.eventBus)
}

func TestLoader_PublishPluginEvent_NoBus(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	require.Error(t, l.PublishPluginEvent(context.Background(), "t", "b", "k", nil))
}

func TestLoader_EventBusPublish_NoBus(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	require.Error(t, l.EventBusPublish(context.Background(), &events.Event{EventType: "t"}))
}

func TestLoader_InvokePluginEvent_PluginNotInstalled(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	sub := &pluginEventSubscription{pluginName: "missing", pattern: "s3:*", handler: "on_event"}
	// Should return without panic.
	l.invokePluginEvent(context.Background(), sub, &events.Event{EventType: "s3:ObjectCreated:Put"}, []byte("{}"))
}
