package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type TracingConfig struct {
	Enabled     bool
	Endpoint    string // OTLP HTTP endpoint, e.g. "localhost:4318"
	ServiceName string
	SampleRatio float64
}

// ShutdownFunc flushes buffered spans; callers must invoke it before exit.
type ShutdownFunc func(context.Context) error

// NewTracerProvider returns a configured *sdktrace.TracerProvider and its
// shutdown function. When cfg.Enabled is false, the provider uses no span
// processors so OTel API calls become no-ops without changing call sites.
func NewTracerProvider(ctx context.Context, cfg TracingConfig) (*sdktrace.TracerProvider, ShutdownFunc, error) {
	// Use schemaless resource to avoid merge conflicts between the SDK's built-in
	// schema URL and any pinned semconv version. service.name is the single
	// attribute we need for Phase 0.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", cfg.ServiceName)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRatio)),
	}

	if cfg.Enabled {
		exporter, err := otlptrace.New(ctx, otlptracehttp.NewClient(
			otlptracehttp.WithEndpoint(cfg.Endpoint),
			otlptracehttp.WithInsecure(),
		))
		if err != nil {
			return nil, nil, fmt.Errorf("create otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
		))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	return tp, tp.Shutdown, nil
}
