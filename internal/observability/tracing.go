package observability

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// TracingConfig holds configuration for OpenTelemetry tracing.
type TracingConfig struct {
	Enabled     bool
	Endpoint    string // OTLP gRPC endpoint
	ServiceName string
	Insecure    bool
}

// InitTracer initializes the OpenTelemetry tracer provider.
// It returns a shutdown function that should be called on application exit.
func InitTracer(cfg *TracingConfig) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return func(ctx context.Context) error { return nil }, nil
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := sdkresource.New(
		context.Background(),
		sdkresource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// TracingHTTPMiddleware creates an HTTP middleware that starts a span for each request
// and propagates trace context via the traceparent header.
func TracingHTTPMiddleware(next http.Handler) http.Handler {
	tracer := otel.Tracer("nexus-gateway")
	propagator := otel.GetTextMapPropagator()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		spanName := fmt.Sprintf("%s %s", r.Method, sanitizePath(r.URL.Path))
		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()

		propagator.Inject(ctx, propagation.HeaderCarrier(w.Header()))

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sanitizePath reduces cardinality of paths for span names.
func sanitizePath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		if len(p) > 0 && (isUUID(p) || isNumeric(p)) {
			parts[i] = ":id"
		}
	}
	return "/" + strings.Join(parts, "/")
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	return s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
