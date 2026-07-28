package pluginloader

import (
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// pluginLogEntry is an in-memory record of a log emitted by a plugin.
type pluginLogEntry struct {
	Level     string
	Payload   string
	Timestamp time.Time
}

// pluginMetricSample is an in-memory record of a metric recorded by a plugin.
type pluginMetricSample struct {
	Name      string
	Value     float64
	Labels    string // JSON
	Timestamp time.Time
}

// pluginDebugBuffer holds recent logs and metrics for a single plugin.
// It is intentionally lossy: old entries are dropped to cap memory.
type pluginDebugBuffer struct {
	mu      sync.RWMutex
	logs    []pluginLogEntry
	metrics []pluginMetricSample
	maxLogs int
	maxMetrics int
}

func newPluginDebugBuffer() *pluginDebugBuffer {
	return &pluginDebugBuffer{
		maxLogs:    1000,
		maxMetrics: 1000,
	}
}

func (b *pluginDebugBuffer) appendLog(level, payload string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logs = append(b.logs, pluginLogEntry{
		Level:     level,
		Payload:   payload,
		Timestamp: time.Now(),
	})
	if len(b.logs) > b.maxLogs {
		b.logs = b.logs[len(b.logs)-b.maxLogs:]
	}
}

func (b *pluginDebugBuffer) appendMetric(name string, value float64, labels string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics = append(b.metrics, pluginMetricSample{
		Name:      name,
		Value:     value,
		Labels:    labels,
		Timestamp: time.Now(),
	})
	if len(b.metrics) > b.maxMetrics {
		b.metrics = b.metrics[len(b.metrics)-b.maxMetrics:]
	}
}

func (b *pluginDebugBuffer) recentLogs(limit int) []pluginLogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if limit <= 0 || limit > len(b.logs) {
		limit = len(b.logs)
	}
	start := len(b.logs) - limit
	if start < 0 {
		start = 0
	}
	out := make([]pluginLogEntry, limit)
	copy(out, b.logs[start:])
	return out
}

func (b *pluginDebugBuffer) recentMetrics(limit int) []pluginMetricSample {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if limit <= 0 || limit > len(b.metrics) {
		limit = len(b.metrics)
	}
	start := len(b.metrics) - limit
	if start < 0 {
		start = 0
	}
	out := make([]pluginMetricSample, limit)
	copy(out, b.metrics[start:])
	return out
}

// debugBuffers manages per-plugin log/metric ring buffers.
type debugBuffers struct {
	mu      sync.RWMutex
	buffers map[string]*pluginDebugBuffer
}

func newDebugBuffers() *debugBuffers {
	return &debugBuffers{buffers: make(map[string]*pluginDebugBuffer)}
}

func (d *debugBuffers) buffer(pluginName string) *pluginDebugBuffer {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.buffers[pluginName]
	if !ok {
		b = newPluginDebugBuffer()
		d.buffers[pluginName] = b
	}
	return b
}

func (d *debugBuffers) recentLogs(pluginName string, limit int) []pluginLogEntry {
	d.mu.RLock()
	b, ok := d.buffers[pluginName]
	d.mu.RUnlock()
	if !ok {
		return nil
	}
	return b.recentLogs(limit)
}

func (d *debugBuffers) recentMetrics(pluginName string, limit int) []pluginMetricSample {
	d.mu.RLock()
	b, ok := d.buffers[pluginName]
	d.mu.RUnlock()
	if !ok {
		return nil
	}
	return b.recentMetrics(limit)
}

// pluginLogLevels holds dynamically-adjustable per-plugin log levels.
type pluginLogLevels struct {
	mu     sync.RWMutex
	levels map[string]zap.AtomicLevel
}

func newPluginLogLevels() *pluginLogLevels {
	return &pluginLogLevels{levels: make(map[string]zap.AtomicLevel)}
}

func (p *pluginLogLevels) set(pluginName, level string) zap.AtomicLevel {
	p.mu.Lock()
	defer p.mu.Unlock()
	lvl := parseLogLevel(level)
	p.levels[pluginName] = lvl
	return lvl
}

func (p *pluginLogLevels) get(pluginName string) zap.AtomicLevel {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if lvl, ok := p.levels[pluginName]; ok {
		return lvl
	}
	return zap.NewAtomicLevelAt(zap.InfoLevel)
}

func (p *pluginLogLevels) enabled(pluginName string, lvl zapcore.Level) bool {
	return p.get(pluginName).Enabled(lvl)
}

// parseLogLevel converts common log-level strings to zap levels.
func parseLogLevel(level string) zap.AtomicLevel {
	switch strings.ToLower(level) {
	case "debug":
		return zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		return zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn", "warning":
		return zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		return zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		return zap.NewAtomicLevelAt(zap.InfoLevel)
	}
}

func zapLevelFromString(level string) zapcore.Level {
	return parseLogLevel(level).Level()
}
