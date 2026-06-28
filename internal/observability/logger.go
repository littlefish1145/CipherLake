package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// LoggingConfig holds configuration for the slog-based logger.
type LoggingConfig struct {
	Level  string
	Format string
}

// redactKeys is the set of key names whose values should be redacted.
var redactKeys = map[string]bool{
	"password": true,
	"secret":   true,
	"key":      true,
	"token":    true,
}

// InitLogger initializes the global slog logger with JSON handler
// and trace_id/span_id extraction support.
func InitLogger(cfg *LoggingConfig) {
	level := parseLevel(cfg.Level)

	handlerOpts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, handlerOpts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, handlerOpts)
	}

	// Wrap handler to add trace_id/span_id from context and redact sensitive keys
	handler = &contextHandler{handler: handler}

	slog.SetDefault(slog.New(handler))
}

// ContextLogger returns a slog.Logger with trace_id and span_id from the context.
func ContextLogger(ctx context.Context) *slog.Logger {
	return slog.Default().With(
		slog.String("trace_id", traceIDFromContext(ctx)),
		slog.String("span_id", spanIDFromContext(ctx)),
	)
}

func traceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	spanCtx := trace.SpanContextFromContext(ctx)
	if spanCtx.HasTraceID() {
		return spanCtx.TraceID().String()
	}
	return ""
}

func spanIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	spanCtx := trace.SpanContextFromContext(ctx)
	if spanCtx.HasSpanID() {
		return spanCtx.SpanID().String()
	}
	return ""
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// contextHandler wraps a slog.Handler to inject trace context and redact sensitive values.
type contextHandler struct {
	handler slog.Handler
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if traceID := traceIDFromContext(ctx); traceID != "" {
			r.AddAttrs(slog.String("trace_id", traceID))
		}
		if spanID := spanIDFromContext(ctx); spanID != "" {
			r.AddAttrs(slog.String("span_id", spanID))
		}
	}
	return h.handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{handler: h.handler.WithAttrs(redactAttrs(attrs))}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{handler: h.handler.WithGroup(name)}
}

// redactAttrs replaces values of sensitive keys with "***REDACTED***".
func redactAttrs(attrs []slog.Attr) []slog.Attr {
	result := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		if shouldRedact(attr.Key) {
			result[i] = slog.String(attr.Key, "***REDACTED***")
		} else {
			result[i] = attr
		}
	}
	return result
}

func shouldRedact(key string) bool {
	lowerKey := strings.ToLower(key)
	for redactKey := range redactKeys {
		if strings.Contains(lowerKey, redactKey) {
			return true
		}
	}
	return false
}
