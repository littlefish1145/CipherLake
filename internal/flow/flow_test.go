package flow

import (
	"context"
	"strings"
	"testing"
	"time"
)

type testPlugin struct {
	name    string
	types   []string
	process func(ctx context.Context, input *ObjectInput) (*ProcessResult, error)
}

func (p *testPlugin) Name() string           { return p.name }
func (p *testPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	return p.process(ctx, input)
}
func (p *testPlugin) CanStream() bool          { return false }
func (p *testPlugin) SupportedTypes() []string { return p.types }

func TestNewTelemetry(t *testing.T) {
	tele := NewTelemetry(DefaultTelemetryConfig())
	if tele == nil {
		t.Fatal("telemetry should not be nil")
	}
	if tele.EventBus == nil {
		t.Error("event bus should not be nil")
	}
	if tele.Logger == nil {
		t.Error("logger should not be nil")
	}
	if tele.Metrics == nil {
		t.Error("metrics should not be nil")
	}
}

func TestEventBusBasic(t *testing.T) {
	bus := NewEventBus(10)
	received := make(chan Event, 5)
	unsub := bus.Subscribe(func(e Event) {
		received <- e
	})
	defer unsub()

	bus.Publish(Event{ID: "1", Type: EventWorkflowStarted, Workflow: "test"})

	select {
	case e := <-received:
		if e.ID != "1" {
			t.Errorf("expected ID 1, got %s", e.ID)
		}
		if e.Type != EventWorkflowStarted {
			t.Errorf("expected EventWorkflowStarted, got %s", e.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestEventBusTypeFilter(t *testing.T) {
	bus := NewEventBus(10)
	var received []Event
	unsub := bus.SubscribeTypes(
		[]EventType{EventWorkflowCompleted, EventWorkflowFailed},
		func(e Event) { received = append(received, e) },
	)
	defer unsub()

	bus.Publish(Event{Type: EventWorkflowStarted})
	bus.Publish(Event{Type: EventWorkflowCompleted})
	bus.Publish(Event{Type: EventStepStarted})
	bus.Publish(Event{Type: EventWorkflowFailed})

	if len(received) != 2 {
		t.Errorf("expected 2 events, got %d", len(received))
	}
}

func TestEventBusUnsubscribe(t *testing.T) {
	bus := NewEventBus(10)
	count := 0
	unsub := bus.Subscribe(func(e Event) { count++ })
	unsub()

	bus.Publish(Event{Type: EventWorkflowStarted})
	if count != 0 {
		t.Error("should not receive events after unsubscribe")
	}
}

func TestExecutorBasic(t *testing.T) {
	tele := NewTelemetry(DefaultTelemetryConfig())
	exec := NewExecutor(10, tele)

	exec.RegisterPlugin(&testPlugin{
		name:  "test",
		types: []string{"*/*"},
		process: func(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
			return &ProcessResult{
				UpdatedMetadata: map[string]string{"processed": "true"},
			}, nil
		},
	})

	err := exec.LoadConfigData([]byte(`pipelines:
  - name: "test-pipeline"
    trigger: "on_upload"
    enabled: true
    steps:
      - name: "step1"
        plugin: "test"
`))
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	matches := exec.GetMatchingPipelines(context.Background(), TriggerOnUpload, "text/plain", nil)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	result, err := exec.Execute(context.Background(), "test-pipeline", &ObjectInput{
		Key: "test.txt", Bucket: "test-bucket", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if result.UpdatedMetadata["processed"] != "true" {
		t.Error("expected processed=true in metadata")
	}
}

func TestExecutorGetMatching(t *testing.T) {
	exec := NewExecutor(10, nil)

	exec.RegisterPlugin(&testPlugin{name: "test", types: []string{"*/*"},
		process: func(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
			return &ProcessResult{}, nil
		},
	})

	exec.LoadConfigData([]byte(`pipelines:
  - name: "img"
    trigger: "on_upload"
    filter: "content-type matches 'image/*'"
    enabled: true
    steps:
      - name: "s1"
        plugin: "test"
  - name: "doc"
    trigger: "on_get"
    enabled: true
    steps:
      - name: "s1"
        plugin: "test"
  - name: "disabled"
    trigger: "on_upload"
    enabled: false
    steps:
      - name: "s1"
        plugin: "test"
`))

	matches := exec.GetMatchingPipelines(context.Background(), TriggerOnUpload, "image/jpeg", nil)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match for image upload, got %d", len(matches))
	}

	matches = exec.GetMatchingPipelines(context.Background(), TriggerOnUpload, "text/plain", nil)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for text upload, got %d", len(matches))
	}

	matches = exec.GetMatchingPipelines(context.Background(), TriggerOnGet, "image/jpeg", nil)
	if len(matches) != 1 {
		t.Errorf("expected 1 match for on_get, got %d", len(matches))
	}
}

func TestExecutorStepParams(t *testing.T) {
	var gotParams map[string]string
	exec := NewExecutor(10, nil)

	exec.RegisterPlugin(&testPlugin{
		name:  "param-test",
		types: []string{"*/*"},
		process: func(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
			gotParams = input.Params
			return &ProcessResult{}, nil
		},
	})

	exec.LoadConfigData([]byte(`pipelines:
  - name: "param-pipeline"
    trigger: "on_upload"
    enabled: true
    steps:
      - name: "s1"
        plugin: "param-test"
        params:
          format: "jpg"
          quality: "90"
`))

	exec.Execute(context.Background(), "param-pipeline", &ObjectInput{
		ContentType: "image/jpeg",
	})

	if gotParams == nil {
		t.Fatal("params should not be nil")
	}
	if gotParams["format"] != "jpg" {
		t.Errorf("expected format=jpg, got %s", gotParams["format"])
	}
	if gotParams["quality"] != "90" {
		t.Errorf("expected quality=90, got %s", gotParams["quality"])
	}
}

func TestExecutorInlineSteps(t *testing.T) {
	exec := NewExecutor(10, nil)
	var stepOrder []string

	exec.RegisterPlugin(&testPlugin{
		name:  "add-header",
		types: []string{"*/*"},
		process: func(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
			stepOrder = append(stepOrder, "add-header")
			return &ProcessResult{Outputs: []*ObjectOutput{{Content: strings.NewReader("header"), Size: 6}}}, nil
		},
	})
	exec.RegisterPlugin(&testPlugin{
		name:  "add-footer",
		types: []string{"*/*"},
		process: func(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
			stepOrder = append(stepOrder, "add-footer")
			return &ProcessResult{Outputs: []*ObjectOutput{{Content: strings.NewReader("header+footer"), Size: 14}}}, nil
		},
	})

	exec.LoadConfigData([]byte(`pipelines:
  - name: "inline-test"
    trigger: "on_upload"
    enabled: true
    steps:
      - name: "header"
        plugin: "add-header"
        inline: true
      - name: "footer"
        plugin: "add-footer"
        inline: true
`))

	result, err := exec.Execute(context.Background(), "inline-test", &ObjectInput{
		Key: "test.txt", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if len(stepOrder) != 2 {
		t.Errorf("expected 2 steps, got %d", len(stepOrder))
	}
	if result != nil && len(result.Outputs) > 0 {
		data := make([]byte, 14)
		result.Outputs[0].Content.Read(data)
		t.Logf("output: %s", string(data))
	}
}

func TestResourceManager(t *testing.T) {
	rm := NewResourceManager(DefaultResourceConfig(), nil)
	if rm == nil {
		t.Fatal("resource manager should not be nil")
	}

	ctx := context.Background()
	if err := rm.Acquire(ctx); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if rm.Active() != 1 {
		t.Errorf("expected 1 active, got %d", rm.Active())
	}

	rm.Release()
	if rm.Active() != 0 {
		t.Errorf("expected 0 active, got %d", rm.Active())
	}
}

func TestResourceManagerCongestion(t *testing.T) {
	cfg := DefaultResourceConfig()
	cfg.MaxWorkers = 1
	rm := NewResourceManager(cfg, nil)

	ctx := context.Background()
	rm.Acquire(ctx)

	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := rm.Acquire(ctx2); err == nil {
		t.Error("expected error on congested acquire")
	}
}

func TestSecrets(t *testing.T) {
	provider := NewEnvSecretProvider()
	provider.Set("api.key", "sk-1234")

	val, ok := provider.Get("api.key")
	if !ok {
		t.Fatal("key not found")
	}
	if val != "sk-1234" {
		t.Errorf("expected sk-1234, got %s", val)
	}

	keys, err := provider.List()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 key, got %d", len(keys))
	}

	provider.Delete("api.key")
	_, ok = provider.Get("api.key")
	if ok {
		t.Error("key should be deleted")
	}
}

func TestSecretsResolve(t *testing.T) {
	provider := NewEnvSecretProvider()
	provider.Set("webhook.url", "https://example.com/hook")
	ws := NewWorkflowSecrets(provider, nil)

	val, ok := ws.Resolve("secret:webhook.url")
	if !ok {
		t.Fatal("secret not resolved")
	}
	if val != "https://example.com/hook" {
		t.Errorf("expected hook url, got %s", val)
	}

	val, ok = ws.Resolve("plain-value")
	if !ok {
		t.Fatal("plain value should resolve")
	}
	if val != "plain-value" {
		t.Errorf("expected plain-value, got %s", val)
	}
}

func TestSecretsResolveMap(t *testing.T) {
	provider := NewEnvSecretProvider()
	provider.Set("db.pass", "secret123")
	ws := NewWorkflowSecrets(provider, nil)

	resolved, err := ws.ResolveMap(map[string]string{
		"url":      "http://db:5432",
		"password": "secret:db.pass",
	})
	if err != nil {
		t.Fatalf("resolve map failed: %v", err)
	}
	if resolved["url"] != "http://db:5432" {
		t.Errorf("expected db url, got %s", resolved["url"])
	}
	if resolved["password"] != "secret123" {
		t.Errorf("expected secret123, got %s", resolved["password"])
	}
}

func TestTimeline(t *testing.T) {
	tl := NewTimeline(100)

	tl.Record("exec-1", Event{Type: EventWorkflowStarted, Time: time.Now(), Workflow: "wf1"})
	tl.Record("exec-1", Event{Type: EventStepStarted, Time: time.Now(), Step: "s1"})
	tl.Record("exec-1", Event{Type: EventWorkflowCompleted, Time: time.Now()})

	events := tl.Get("exec-1")
	if len(events) != 3 {
		t.Errorf("expected 3 events, got %d", len(events))
	}

	ids := tl.List(10)
	if len(ids) != 1 {
		t.Errorf("expected 1 execution in list, got %d", len(ids))
	}
}

func TestHealth(t *testing.T) {
	rm := NewResourceManager(DefaultResourceConfig(), nil)
	h := NewHealth(rm, nil)

	report := h.Report()
	if report.Status != HealthOK {
		t.Errorf("expected ok status, got %s", report.Status)
	}
	if report.Workers.Max <= 0 {
		t.Errorf("expected max workers > 0, got %d", report.Workers.Max)
	}
}

func TestHealthDegraded(t *testing.T) {
	rm := NewResourceManager(DefaultResourceConfig(), nil)
	h := NewHealth(rm, nil)

	h.RegisterChecker("storage", func() HealthCheck {
		return HealthCheck{Name: "storage", Status: HealthDegraded}
	})

	report := h.Report()
	if report.Status != HealthDegraded {
		t.Errorf("expected degraded status, got %s", report.Status)
	}
}

func TestTimelineCapture(t *testing.T) {
	tl := NewTimeline(100)
	sub := newTimelineSubscriber(tl)

	sub(Event{
		Execution: "e1",
		Type:      EventWorkflowStarted,
		Time:      time.Now(),
		Workflow:  "wf1",
		Step:      "",
	})

	events := tl.Get("e1")
	if len(events) != 1 {
		t.Errorf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != EventWorkflowStarted {
		t.Errorf("expected EventWorkflowStarted, got %s", events[0].Type)
	}
}
