package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestTracing_StartsSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var spanInsideHandler trace.SpanContext
	h := Tracing("x-beacon")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		spanInsideHandler = trace.SpanContextFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.True(t, spanInsideHandler.IsValid(), "handler must observe an active span")

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	// otelhttp uses the operation name as the span name when no formatter is set.
	assert.Contains(t, spans[0].Name, "x-beacon.http")
}

func TestTracing_NoOpProviderDoesNotPanic(t *testing.T) {
	// Default global provider is a noop unless main installs one. Verify the
	// middleware survives that environment so disabling tracing in config is
	// safe.
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(otel.GetTracerProvider()) // re-set the noop
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	h := Tracing("x-beacon")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() { h.ServeHTTP(rec, req) })
	assert.Equal(t, http.StatusOK, rec.Code)
}
