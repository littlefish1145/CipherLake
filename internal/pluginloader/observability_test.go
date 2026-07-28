package pluginloader

import (
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func overflowValue(gatherer prometheus.Gatherer, pluginName, metricName string) float64 {
	families, err := gatherer.Gather()
	if err != nil {
		return 0
	}
	for _, f := range families {
		if f.GetName() != "cipherlake_plugin_metric_cardinality_overflow_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["plugin_name"] == pluginName && labels["metric_name"] == metricName {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestHostImports_MetricRecord_CardinalityLimits(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("metrics-plugin", 0, []string{"metric:record"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	name := []byte("events_total")
	writeTestMemory(t, mem, base, name)

	before := overflowValue(prometheus.DefaultGatherer, "metrics-plugin", "metric_overflow")

	// Record 50 distinct metric names (within limit).
	for i := 0; i < maxMetricNamesPerPlugin; i++ {
		labels := map[string]string{"idx": string(rune('a' + i%26))}
		labelsJSON, _ := json.Marshal(labels)
		writeTestMemory(t, mem, base+128, labelsJSON)
		metricName := []byte("metric_" + string(rune('a'+i)))
		writeTestMemory(t, mem, base, metricName)
		h.metricRecord(ctx, mod, uint64(tok),
			base, uint32(len(metricName)),
			1,
			base+128, uint32(len(labelsJSON)),
		)
	}

	// 51st metric name should be dropped and increment overflow counter.
	metricName := []byte("metric_overflow")
	writeTestMemory(t, mem, base, metricName)
	labelsJSON, _ := json.Marshal(map[string]string{"idx": "z"})
	writeTestMemory(t, mem, base+128, labelsJSON)
	h.metricRecord(ctx, mod, uint64(tok),
		base, uint32(len(metricName)),
		1,
		base+128, uint32(len(labelsJSON)),
	)

	after := overflowValue(prometheus.DefaultGatherer, "metrics-plugin", "metric_overflow")
	require.Equal(t, before+1, after, "overflow counter should increment by one")
}

func TestHostImports_TraceSpanLifecycle(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("trace-plugin", 0, []string{"trace:span"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	name := []byte("plugin-op")
	attrs := []byte(`{"key":"value"}`)
	writeTestMemory(t, mem, base, name)
	writeTestMemory(t, mem, base+128, attrs)

	h.traceSpanStart(ctx, mod, uint64(tok),
		base, uint32(len(name)),
		base+128, uint32(len(attrs)),
		base+256,
	)
	handle := readTestUint64(t, mem, base+256)
	require.Greater(t, handle, uint64(0), "span handle should be non-zero")

	// End should succeed and remove the span.
	h.traceSpanEnd(ctx, mod, uint64(tok), handle)
	require.Empty(t, h.pluginTracer.spans, "span should be removed after end")
}

func TestHostImports_LogEmit_ForwardsToSlog(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	ctx := testCtx(t)
	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("log-plugin", 0, []string{"log:emit"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	level := []byte("info")
	payload := []byte(`{"msg":"hello","count":1}`)
	writeTestMemory(t, mem, base, level)
	writeTestMemory(t, mem, base+128, payload)

	// Should not panic and should buffer the log entry.
	h.logEmit(ctx, mod, uint64(tok),
		base, uint32(len(level)),
		base+128, uint32(len(payload)),
	)

	entries := l.RecentDebugLogs("log-plugin", 10)
	require.Len(t, entries, 1)
	require.Equal(t, "info", entries[0].Level)
	require.Equal(t, string(payload), entries[0].Payload)
}

func TestPluginMetrics_LabelSetLimit(t *testing.T) {
	pm := NewPluginMetrics()
	name := "same_metric"

	// 20 distinct label combinations should be accepted.
	for i := 0; i < maxLabelSetsPerMetric; i++ {
		require.True(t, pm.Record("p", name, 1, map[string]string{"idx": string(rune('a' + i))}))
	}
	// 21st should be dropped.
	require.False(t, pm.Record("p", name, 1, map[string]string{"idx": "overflow"}))
}
