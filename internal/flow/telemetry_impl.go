package flow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type channelEventBus struct {
	mu          sync.RWMutex
	subscribers []EventSubscriber
	typeSubs    []struct {
		types []EventType
		fn    EventSubscriber
	}
	dropped int
	closed  bool
}

func NewEventBus(buffer int) EventBus {
	if buffer <= 0 {
		buffer = 256
	}
	return &channelEventBus{}
}

func (b *channelEventBus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, s := range b.subscribers {
		s(e)
	}
	for _, ts := range b.typeSubs {
		for _, t := range ts.types {
			if t == e.Type {
				ts.fn(e)
				break
			}
		}
	}
}

func (b *channelEventBus) Subscribe(fn EventSubscriber) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers = append(b.subscribers, fn)
	idx := len(b.subscribers) - 1
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.subscribers = append(b.subscribers[:idx], b.subscribers[idx+1:]...)
	}
}

func (b *channelEventBus) SubscribeTypes(types []EventType, fn EventSubscriber) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	ts := struct {
		types []EventType
		fn    EventSubscriber
	}{types, fn}
	b.typeSubs = append(b.typeSubs, ts)
	idx := len(b.typeSubs) - 1
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.typeSubs = append(b.typeSubs[:idx], b.typeSubs[idx+1:]...)
	}
}

func (b *channelEventBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func NewTelemetry(cfg TelemetryConfig) *Telemetry {
	bus := NewEventBus(256)
	logger := newZapLogger(cfg.LoggerConfig)
	metrics := newNopMetrics()

	if cfg.MetricsConfig.Enabled {
		metrics = newPromMetrics(cfg.MetricsConfig)
	}

	var tracer Tracer
	if cfg.TracingEnabled {
		tracer = newNopTracer()
	} else {
		tracer = newNopTracer()
	}

	var audit AuditLog
	if cfg.AuditEnabled {
		audit = newNopAudit()
	} else {
		audit = newNopAudit()
	}

	t := &Telemetry{
		EventBus: bus,
		Logger:   logger,
		Metrics:  metrics,
		Tracer:   tracer,
		Audit:    audit,
	}

	bus.Subscribe(newMetricsSubscriber(metrics))
	bus.Subscribe(newLogSubscriber(logger))

	return t
}

func newZapLogger(cfg LoggerConfig) Logger {
	level := zapcore.InfoLevel
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = zapcore.DebugLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	}

	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "time"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.AddSync(os.Stdout),
		level,
	)

	return &zapLoggerAdapter{zap.New(core, zap.AddCallerSkip(1)).Sugar()}
}

type zapLoggerAdapter struct {
	*zap.SugaredLogger
}

func (l *zapLoggerAdapter) Debug(msg string, fields ...any) { l.SugaredLogger.Debugw(msg, fields...) }
func (l *zapLoggerAdapter) Info(msg string, fields ...any)  { l.SugaredLogger.Infow(msg, fields...) }
func (l *zapLoggerAdapter) Warn(msg string, fields ...any)  { l.SugaredLogger.Warnw(msg, fields...) }
func (l *zapLoggerAdapter) Error(msg string, fields ...any) { l.SugaredLogger.Errorw(msg, fields...) }

func (l *zapLoggerAdapter) With(fields ...any) Logger {
	return &zapLoggerAdapter{l.SugaredLogger.With(fields...)}
}

func newLogSubscriber(log Logger) EventSubscriber {
	return func(e Event) {
		fields := []any{
			"event_id", e.ID,
			"event_type", string(e.Type),
			"workflow", e.Workflow,
			"step", e.Step,
			"execution", e.Execution,
			"worker", e.Worker,
			"duration", e.Duration.String(),
			"retry", e.Retry,
		}
		if e.Error != "" {
			fields = append(fields, "error", e.Error)
		}
		if e.Actor != "" {
			fields = append(fields, "actor", e.Actor)
		}

		msg := fmt.Sprintf("flow: %s", e.Type)
		switch {
		case e.Error != "" && isErrorLevel(e.Type):
			log.Error(msg, fields...)
		case isWarnLevel(e.Type):
			log.Warn(msg, fields...)
		default:
			log.Info(msg, fields...)
		}
	}
}

func isErrorLevel(t EventType) bool {
	switch t {
	case EventWorkflowFailed, EventStepFailed, EventPluginFailed,
		EventRecoveryFailed, EventTimerMissed:
		return true
	}
	return false
}

func isWarnLevel(t EventType) bool {
	switch t {
	case EventStepRetried, EventQueueBacklog, EventWorkerBusy:
		return true
	}
	return false
}

func newMetricsSubscriber(metrics MetricsRecorder) EventSubscriber {
	return func(e Event) {
		switch e.Type {
		case EventWorkflowStarted:
			metrics.CounterInc("workflow_started", "workflow", e.Workflow)
			metrics.GaugeInc("workflow_running", "workflow", e.Workflow)
		case EventWorkflowCompleted:
			metrics.GaugeDec("workflow_running", "workflow", e.Workflow)
			metrics.CounterInc("workflow_completed", "workflow", e.Workflow)
			metrics.HistogramObserve("workflow_duration", e.Duration.Seconds(), "workflow", e.Workflow)
		case EventWorkflowFailed:
			metrics.GaugeDec("workflow_running", "workflow", e.Workflow)
			metrics.CounterInc("workflow_failed", "workflow", e.Workflow)
		case EventStepStarted:
			metrics.CounterInc("step_started", "step", e.Step)
		case EventStepCompleted:
			metrics.CounterInc("step_completed", "step", e.Step)
			metrics.HistogramObserve("step_duration", e.Duration.Seconds(), "step", e.Step)
		case EventStepFailed:
			metrics.CounterInc("step_failed", "step", e.Step)
		case EventStepRetried:
			metrics.CounterInc("step_retried", "step", e.Step)
		case EventWorkerBusy:
			metrics.GaugeInc("worker_busy")
		case EventWorkerIdle:
			metrics.GaugeDec("worker_busy")
		}
	}
}

type nopMetrics struct{}

func newNopMetrics() MetricsRecorder { return &nopMetrics{} }
func (n *nopMetrics) CounterInc(name string, labels ...string)      {}
func (n *nopMetrics) CounterAdd(name string, v float64, l ...string) {}
func (n *nopMetrics) HistogramObserve(name string, v float64, l ...string) {}
func (n *nopMetrics) GaugeSet(name string, v float64, l ...string)    {}
func (n *nopMetrics) GaugeInc(name string, labels ...string)          {}
func (n *nopMetrics) GaugeDec(name string, labels ...string)          {}

type nopTracer struct{}

func newNopTracer() Tracer { return &nopTracer{} }
func (n *nopTracer) StartSpan(ctx context.Context, name string, opts ...SpanOption) (context.Context, Span) {
	return ctx, &nopSpan{}
}

type nopSpan struct{}
func (n *nopSpan) End()                                       {}
func (n *nopSpan) SetError(err error)                         {}
func (n *nopSpan) SetAttribute(key, value string)             {}
func (n *nopSpan) AddEvent(name string, attrs map[string]string) {}

type nopAudit struct{}

func newNopAudit() AuditLog { return &nopAudit{} }
func (n *nopAudit) Record(entry AuditEntry)                       {}
func (n *nopAudit) Query(from, to time.Time, limit int) ([]AuditEntry, error) { return nil, nil }
func (n *nopAudit) Close() error                                  { return nil }

type promMetrics struct {
	counters   map[string]promCounter
	gauges     map[string]promGauge
	histograms map[string]promHistogram
	prefix     string
	mu         sync.RWMutex
}

type promCounter  struct{}
type promGauge    struct{}
type promHistogram struct{}

func newPromMetrics(cfg MetricsConfig) MetricsRecorder {
	return &promMetrics{
		counters:   make(map[string]promCounter),
		gauges:     make(map[string]promGauge),
		histograms: make(map[string]promHistogram),
		prefix:     cfg.Prefix,
	}
}

func (p *promMetrics) key(name string, labels []string) string {
	return p.prefix + "_" + name + "{" + strings.Join(labels, ",") + "}"
}

func (p *promMetrics) CounterInc(name string, labels ...string)      { p.CounterAdd(name, 1, labels...) }
func (p *promMetrics) CounterAdd(name string, v float64, l ...string) {}
func (p *promMetrics) HistogramObserve(name string, v float64, l ...string) {}
func (p *promMetrics) GaugeSet(name string, v float64, l ...string)    {}
func (p *promMetrics) GaugeInc(name string, labels ...string)          {}
func (p *promMetrics) GaugeDec(name string, labels ...string)          {}
