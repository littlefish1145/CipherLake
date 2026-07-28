package flow

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type ExporterType string

const (
	ExporterConsole   ExporterType = "console"
	ExporterPrometheus ExporterType = "prometheus"
	ExporterOTLP      ExporterType = "otlp"
	ExporterFile      ExporterType = "file"
	ExporterWebhook   ExporterType = "webhook"
)

type Exporter interface {
	Name() string
	Export(event Event) error
	Close() error
}

type ExportManager struct {
	mu        sync.RWMutex
	exporters map[string]Exporter
	eventBus  EventBus
	unsub     func()
}

func NewExportManager(eventBus EventBus) *ExportManager {
	m := &ExportManager{
		exporters: make(map[string]Exporter),
		eventBus:  eventBus,
	}
	unsub := eventBus.Subscribe(func(e Event) {
		m.mu.RLock()
		defer m.mu.RUnlock()
		for _, ex := range m.exporters {
			ex.Export(e)
		}
	})
	m.unsub = unsub
	return m
}

func (m *ExportManager) Register(exporter Exporter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exporters[exporter.Name()] = exporter
}

func (m *ExportManager) Unregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.exporters, name)
}

func (m *ExportManager) Close() {
	m.unsub()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.exporters {
		ex.Close()
	}
}

type consoleExporter struct{}

func NewConsoleExporter() Exporter { return &consoleExporter{} }

func (c *consoleExporter) Name() string { return "console" }

func (c *consoleExporter) Export(e Event) error {
	fmt.Printf("[flow] %s | %s | workflow=%s step=%s exec=%s dur=%s err=%s\n",
		e.Time.Format(time.RFC3339Nano), e.Type, e.Workflow, e.Step, e.Execution, e.Duration, e.Error)
	return nil
}

func (c *consoleExporter) Close() error { return nil }

type promExporter struct {
	mu         sync.RWMutex
	counters   map[string]int64
	histograms map[string][]float64
	gauges     map[string]int64
	prefix     string
}

func NewPromExporter(prefix string) Exporter {
	if prefix == "" {
		prefix = "nexus_flow"
	}
	return &promExporter{
		counters:   make(map[string]int64),
		histograms: make(map[string][]float64),
		gauges:     make(map[string]int64),
		prefix:     prefix,
	}
}

func (p *promExporter) Name() string { return "prometheus" }

func (p *promExporter) Export(e Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := func(name string, labels ...string) string {
		labelStr := strings.Join(labels, ",")
		if labelStr != "" {
			return p.prefix + "_" + name + "{" + labelStr + "}"
		}
		return p.prefix + "_" + name
	}

	switch e.Type {
	case EventWorkflowStarted:
		p.counters[key("workflow_started", "workflow", e.Workflow)]++
		p.gauges[key("workflow_running", "workflow", e.Workflow)]++
	case EventWorkflowCompleted:
		p.gauges[key("workflow_running", "workflow", e.Workflow)]--
		p.counters[key("workflow_completed", "workflow", e.Workflow)]++
		p.histograms[key("workflow_duration", "workflow", e.Workflow)] = append(
			p.histograms[key("workflow_duration", "workflow", e.Workflow)], e.Duration.Seconds())
	case EventWorkflowFailed:
		p.gauges[key("workflow_running", "workflow", e.Workflow)]--
		p.counters[key("workflow_failed", "workflow", e.Workflow)]++
	}
	return nil
}

func (p *promExporter) Snapshot() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var lines []string
	lines = append(lines, "# HELP nexus_flow metrics", "# TYPE nexus_flow metrics")

	for k, v := range p.counters {
		lines = append(lines, fmt.Sprintf("# TYPE %s counter", k))
		lines = append(lines, fmt.Sprintf("%s %d", k, v))
	}

	for k, v := range p.gauges {
		lines = append(lines, fmt.Sprintf("# TYPE %s gauge", k))
		lines = append(lines, fmt.Sprintf("%s %d", k, v))
	}

	for k, vals := range p.histograms {
		if len(vals) == 0 {
			continue
		}
		sort.Float64s(vals)
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		lines = append(lines, fmt.Sprintf("# TYPE %s histogram", k))
		lines = append(lines, fmt.Sprintf("%s_count %d", k, len(vals)))
		lines = append(lines, fmt.Sprintf("%s_sum %f", k, sum))
		for i, v := range vals {
			lines = append(lines, fmt.Sprintf("%s_bucket{le=\"%f\"} %d", k, v, i+1))
		}
		lines = append(lines, fmt.Sprintf("%s_bucket{le=\"+Inf\"} %d", k, len(vals)))
	}

	return strings.Join(lines, "\n")
}

func (p *promExporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(p.Snapshot()))
}

func (p *promExporter) Close() error { return nil }

func (p *promExporter) Handler() http.HandlerFunc {
	return p.ServeHTTP
}
