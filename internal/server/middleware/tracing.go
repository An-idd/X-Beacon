package middleware

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Tracing wraps the handler chain in an OTel span. Span name defaults to
// "<service>.http" because chi's route pattern isn't known until after the
// mux dispatches; the route-aware rename can be added later by the
// /v1/chat/completions handlers themselves via trace.SpanFromContext.
//
// The TracerProvider is read from the global set in main (otel.SetTracerProvider).
// Disabling tracing in config installs a no-op provider, so this middleware
// is always safe to mount.
func Tracing(serviceName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, serviceName+".http")
	}
}
