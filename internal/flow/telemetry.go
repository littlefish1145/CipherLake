package flow

import (
	"context"
	"time"
)

type EventType string

const (
	EventWorkflowCreated   EventType = "workflow.created"
	EventWorkflowStarted   EventType = "workflow.started"
	EventWorkflowCompleted EventType = "workflow.completed"
	EventWorkflowFailed    EventType = "workflow.failed"
	EventWorkflowCancelled EventType = "workflow.cancelled"

	EventStepStarted   EventType = "step.started"
	EventStepCompleted EventType = "step.completed"
	EventStepFailed    EventType = "step.failed"
	EventStepSkipped   EventType = "step.skipped"
	EventStepRetried   EventType = "step.retried"

	EventPluginLoaded   EventType = "plugin.loaded"
	EventPluginRemoved  EventType = "plugin.removed"
	EventPluginFailed   EventType = "plugin.failed"

	EventWorkerBusy    EventType = "worker.busy"
	EventWorkerIdle    EventType = "worker.idle"
	EventQueueBacklog  EventType = "queue.backlog"

	EventSecretUsed    EventType = "secret.used"
	EventSecretChanged EventType = "secret.changed"

	EventConfigUpdated EventType = "config.updated"
	EventConfigDeleted EventType = "config.deleted"

	EventTimerTriggered EventType = "timer.triggered"
	EventTimerMissed    EventType = "timer.missed"

	EventRecoveryStarted  EventType = "recovery.started"
	EventRecoveryComplete EventType = "recovery.complete"
	EventRecoveryFailed   EventType = "recovery.failed"

	EventAuditAccess EventType = "audit.access"
)

type Event struct {
	ID        string
	Type      EventType
	Time      time.Time
	Workflow  string
	Step      string
	Execution string
	Worker    int
	Duration  time.Duration
	Error     string
	Retry     int
	OldValue  string
	NewValue  string
	Actor     string
	Metadata  map[string]string
}

type EventSubscriber func(Event)

type EventBus interface {
	Publish(Event)
	Subscribe(EventSubscriber) func()
	SubscribeTypes(types []EventType, handler EventSubscriber) func()
	Close() error
}

type Logger interface {
	Debug(msg string, fields ...any)
	Info(msg string, fields ...any)
	Warn(msg string, fields ...any)
	Error(msg string, fields ...any)
	With(fields ...any) Logger
}

type MetricsRecorder interface {
	CounterInc(name string, labels ...string)
	CounterAdd(name string, value float64, labels ...string)
	HistogramObserve(name string, value float64, labels ...string)
	GaugeSet(name string, value float64, labels ...string)
	GaugeInc(name string, labels ...string)
	GaugeDec(name string, labels ...string)
}

type Tracer interface {
	StartSpan(ctx context.Context, name string, opts ...SpanOption) (context.Context, Span)
}

type SpanOption func(*SpanConfig)

type SpanConfig struct {
	Workflow  string
	Step      string
	Execution string
	Attributes map[string]string
}

type Span interface {
	End()
	SetError(err error)
	SetAttribute(key, value string)
	AddEvent(name string, attrs map[string]string)
}

type AuditEntry struct {
	Time       time.Time
	Actor      string
	Action     string
	Resource   string
	OldValue   string
	NewValue   string
	RemoteAddr string
	UserAgent  string
}

type AuditLog interface {
	Record(entry AuditEntry)
	Query(from, to time.Time, limit int) ([]AuditEntry, error)
	Close() error
}

type Telemetry struct {
	EventBus EventBus
	Logger   Logger
	Metrics  MetricsRecorder
	Tracer   Tracer
	Audit    AuditLog
}

type TelemetryOption func(*TelemetryConfig)

type TelemetryConfig struct {
	LoggerConfig LoggerConfig
	MetricsConfig
	TracingEnabled bool
	AuditEnabled   bool
	OTLPEndpoint   string
	ServiceName    string
}

type LoggerConfig struct {
	Level       string
	OutputPaths []string
	Development bool
}

type MetricsConfig struct {
	Enabled     bool
	Prefix      string
	Buckets     []float64
}

func DefaultTelemetryConfig() TelemetryConfig {
	return TelemetryConfig{
		LoggerConfig: LoggerConfig{
			Level:       "info",
			OutputPaths: []string{"stdout"},
		},
		MetricsConfig: MetricsConfig{
			Enabled: true,
			Prefix:  "nexus_flow",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		TracingEnabled: false,
		AuditEnabled:   true,
		ServiceName:    "nexus-flow",
	}
}
