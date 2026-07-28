package pluginloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Cardinality limits for plugin-emitted metrics (spec §3.17).
const (
	maxMetricNamesPerPlugin = 50
	maxLabelSetsPerMetric   = 20
)

// pluginOverflowCounter is a process-level singleton so that all
// PluginMetrics instances (including those created in tests) contribute to
// the same exported metric family.
var pluginOverflowCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "cipherlake",
		Name:      "plugin_metric_cardinality_overflow_total",
		Help:      "Dropped plugin metrics due to cardinality limits",
	},
	[]string{"plugin_name", "metric_name"},
)

func init() {
	_ = prometheus.DefaultRegisterer.Register(pluginOverflowCounter)
}

// PluginMetrics buffers and forwards plugin-emitted metrics to Prometheus
// while enforcing cardinality limits.
//
// Each (plugin_name, metric_name) pair becomes a dedicated CounterVec whose
// label keys are fixed from the first observed sample. Distinct label value
// combinations are capped at maxLabelSetsPerMetric; exceeding samples are
// dropped and counted by the overflow counter.
type PluginMetrics struct {
	mu             sync.RWMutex
	metrics        map[string]*pluginMetric // key = pluginName + "/" + metricName
	namesPerPlugin map[string]map[string]bool
}

type pluginMetric struct {
	counter   *prometheus.CounterVec
	labelKeys []string
	labelSets map[string]bool // key = sorted label-values JSON
}

// NewPluginMetrics constructs an empty plugin metrics recorder.
func NewPluginMetrics() *PluginMetrics {
	return &PluginMetrics{
		metrics:        make(map[string]*pluginMetric),
		namesPerPlugin: make(map[string]map[string]bool),
	}
}

// Register is a no-op kept for API compatibility. The overflow counter is
// registered once in init().
func (pm *PluginMetrics) Register(reg prometheus.Registerer) error {
	return nil
}

// Record emits a plugin metric sample. Returns true if the sample was
// accepted (within cardinality limits) or false if it was dropped.
func (pm *PluginMetrics) Record(pluginName, metricName string, value float64, labels map[string]string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	names, ok := pm.namesPerPlugin[pluginName]
	if !ok {
		names = make(map[string]bool)
		pm.namesPerPlugin[pluginName] = names
	}
	if !names[metricName] {
		if len(names) >= maxMetricNamesPerPlugin {
			pluginOverflowCounter.WithLabelValues(pluginName, metricName).Inc()
			return false
		}
		names[metricName] = true
	}

	key := pluginName + "/" + metricName
	pmMetric, ok := pm.metrics[key]
	if !ok {
		labelKeys := sortedLabelKeys(labels)
		counter := prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "cipherlake",
				Name:      "plugin_metric_record_total",
				Help:      "Records emitted by plugins",
				ConstLabels: prometheus.Labels{
					"plugin_name": pluginName,
					"metric_name": metricName,
				},
			},
			labelKeys,
		)
		pm.metrics[key] = &pluginMetric{
			counter:   counter,
			labelKeys: labelKeys,
			labelSets: make(map[string]bool),
		}
		pmMetric = pm.metrics[key]
		// Register best-effort; ignore already-registered errors from tests
		// that may recreate the loader.
		_ = prometheus.DefaultRegisterer.Register(counter)
	}

	if !labelKeysMatch(pmMetric.labelKeys, labels) {
		pluginOverflowCounter.WithLabelValues(pluginName, metricName).Inc()
		return false
	}

	labelSetKey := labelValuesKey(pmMetric.labelKeys, labels)
	if !pmMetric.labelSets[labelSetKey] {
		if len(pmMetric.labelSets) >= maxLabelSetsPerMetric {
			pluginOverflowCounter.WithLabelValues(pluginName, metricName).Inc()
			return false
		}
		pmMetric.labelSets[labelSetKey] = true
	}

	values := make([]string, len(pmMetric.labelKeys))
	for i, k := range pmMetric.labelKeys {
		values[i] = labels[k]
	}
	pmMetric.counter.WithLabelValues(values...).Add(value)
	return true
}

func sortedLabelKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func labelKeysMatch(keys []string, labels map[string]string) bool {
	if len(keys) != len(labels) {
		return false
	}
	for _, k := range keys {
		if _, ok := labels[k]; !ok {
			return false
		}
	}
	return true
}

func labelValuesKey(keys []string, labels map[string]string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}

// PluginTracer manages OpenTelemetry spans created by plugins.
//
// Spans created by a plugin via trace_span_start are children of the
// active trace context passed to the host function by wazero (if any).
// The returned handle is an opaque u64 that the plugin later passes to
// trace_span_end.
type PluginTracer struct {
	mu     sync.Mutex
	nextID uint64
	spans  map[uint64]trace.Span
}

// NewPluginTracer constructs an empty tracer.
func NewPluginTracer() *PluginTracer {
	return &PluginTracer{spans: make(map[uint64]trace.Span)}
}

// Start creates a new span as a child of ctx and returns a handle.
// Returns 0 and an error if the span could not be created.
func (pt *PluginTracer) Start(ctx context.Context, pluginName, spanName string, attrs []attribute.KeyValue) (uint64, trace.Span, error) {
	tracer := trace.SpanFromContext(ctx).TracerProvider().Tracer("cipherlake-plugin")
	ctx, span := tracer.Start(ctx, spanName)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	span.SetAttributes(attribute.String("plugin_name", pluginName))

	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.nextID++
	if pt.nextID == 0 {
		pt.nextID++
	}
	id := pt.nextID
	pt.spans[id] = span
	return id, span, nil
}

// End finishes the span associated with handle. Returns an error if the
// handle is unknown.
func (pt *PluginTracer) End(handle uint64) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	span, ok := pt.spans[handle]
	if !ok {
		return fmt.Errorf("trace span handle %d not found", handle)
	}
	delete(pt.spans, handle)
	span.End()
	return nil
}

// pluginLogAttrs attempts to parse a JSON payload into slog attributes.
// Non-JSON payloads are returned as a single message attribute.
func pluginLogAttrs(payload string) []any {
	if payload == "" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return []any{slog.String("message", payload)}
	}
	attrs := make([]any, 0, len(obj))
	for k, v := range obj {
		attrs = append(attrs, slog.Any(k, v))
	}
	return attrs
}

func parseSlogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// atomic helpers for tracing.
var _ = atomic.AddUint64 // ensure sync/atomic stays imported meaningfully

var (
	_ = errors.New // keep errors imported
)
