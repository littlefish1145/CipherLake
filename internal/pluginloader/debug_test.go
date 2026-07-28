package pluginloader

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestDebugBuffer_AppendAndRecentLogs(t *testing.T) {
	b := newPluginDebugBuffer()
	b.maxLogs = 5
	for i := 0; i < 10; i++ {
		b.appendLog("info", "msg-"+string(rune('0'+i)))
	}
	logs := b.recentLogs(3)
	require.Len(t, logs, 3)
	require.Equal(t, "msg-7", logs[0].Payload)
	require.Equal(t, "msg-9", logs[2].Payload)
}

func TestDebugBuffer_AppendAndRecentMetrics(t *testing.T) {
	b := newPluginDebugBuffer()
	b.maxMetrics = 3
	b.appendMetric("cpu", 0.1, `{"x":"a"}`)
	b.appendMetric("cpu", 0.2, `{"x":"b"}`)
	b.appendMetric("cpu", 0.3, `{"x":"c"}`)
	b.appendMetric("cpu", 0.4, `{"x":"d"}`)
	metrics := b.recentMetrics(2)
	require.Len(t, metrics, 2)
	require.InDelta(t, 0.3, metrics[0].Value, 0.001)
	require.InDelta(t, 0.4, metrics[1].Value, 0.001)
}

func TestPluginLogLevels_SetAndGet(t *testing.T) {
	levels := newPluginLogLevels()
	levels.set("demo", "debug")
	require.Equal(t, zap.DebugLevel, levels.get("demo").Level())
	require.Equal(t, zap.InfoLevel, levels.get("other").Level())
}

func TestLoader_DebugLogLevelFiltering(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	l.SetPluginLogLevel("demo", "error")
	l.DebugLog("demo", "info", "ignored")
	l.DebugLog("demo", "error", "recorded")

	logs := l.RecentDebugLogs("demo", 10)
	require.Len(t, logs, 1)
	require.Equal(t, "error", logs[0].Level)
	require.Equal(t, "recorded", logs[0].Payload)
}

func TestLoader_DebugMetric(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	l.DebugMetric("demo", "cpu", 0.5, `{"core":"0"}`)
	metrics := l.RecentDebugMetrics("demo", 10)
	require.Len(t, metrics, 1)
	require.Equal(t, "cpu", metrics[0].Name)
	require.InDelta(t, 0.5, metrics[0].Value, 0.001)
	require.Equal(t, `{"core":"0"}`, metrics[0].Labels)
}

func TestLoader_StateDump(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	_, err := l.StatePut("demo", "k1", []byte("v1"), 0)
	require.NoError(t, err)
	_, err = l.StatePut("demo", "k2", []byte("v2"), 0)
	require.NoError(t, err)

	dump, err := l.StateDump("demo")
	require.NoError(t, err)
	require.Len(t, dump, 2)
	require.Equal(t, []byte("v1"), dump["k1"].Value)
	require.Equal(t, []byte("v2"), dump["k2"].Value)
	require.NotZero(t, dump["k1"].Version)
}
