package middleware

import (
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// LoggingOptions configures the Logging middleware.
type LoggingOptions struct {
	// SkipPaths is the set of exact request paths whose access logs should
	// be suppressed. Typical entries: "/metrics" (Prometheus scrapes are
	// noisy), "/healthz" (k8s probes). Match is exact, not prefix.
	SkipPaths []string
}

// Logging emits one access log line per request after the handler returns.
// The line carries method/path/status/latency/req_id and (when present) the
// trace_id from the active span. The request ID is read from context, so
// RequestID middleware must be mounted earlier in the chain.
//
// Output side effects: zap.Info (or zap.Warn for 4xx, zap.Error for 5xx).
// Skip-listed paths emit nothing.
func Logging(logger *zap.Logger, opts LoggingOptions) func(http.Handler) http.Handler {
	skip := make(map[string]struct{}, len(opts.SkipPaths))
	for _, p := range opts.SkipPaths {
		skip[p] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, skipIt := skip[r.URL.Path]; skipIt {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			lrw := newLoggingResponseWriter(w)
			next.ServeHTTP(lrw, r)
			latency := time.Since(start)

			fields := []zap.Field{
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", lrw.status),
				zap.Int("bytes", lrw.bytes),
				zap.Duration("latency", latency),
				zap.String("remote", r.RemoteAddr),
				zap.String("req_id", RequestIDFrom(r.Context())),
			}
			if span := trace.SpanFromContext(r.Context()); span.SpanContext().IsValid() {
				fields = append(fields, zap.String("trace_id", span.SpanContext().TraceID().String()))
			}

			switch {
			case lrw.status >= 500:
				logger.Error("http request", fields...)
			case lrw.status >= 400:
				logger.Warn("http request", fields...)
			default:
				logger.Info("http request", fields...)
			}
		})
	}
}

// loggingResponseWriter captures status and bytes written for the access
// log. It exposes http.Flusher so Step 3.5's SSE writer keeps working
// through the middleware chain — calling Flush on the wrapper forwards to
// the underlying writer iff it supports Flusher.
type loggingResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func newLoggingResponseWriter(w http.ResponseWriter) *loggingResponseWriter {
	// Default 200 matches net/http: when a handler calls Write without
	// WriteHeader, the implicit status is 200.
	return &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	if lrw.wroteHeader {
		return
	}
	lrw.wroteHeader = true
	lrw.status = code
	lrw.ResponseWriter.WriteHeader(code)
}

func (lrw *loggingResponseWriter) Write(b []byte) (int, error) {
	if !lrw.wroteHeader {
		lrw.wroteHeader = true
	}
	n, err := lrw.ResponseWriter.Write(b)
	lrw.bytes += n
	return n, err
}

// Flush forwards to the underlying writer if it implements http.Flusher.
// SSE handlers (Step 3.5) rely on this for chunk-by-chunk delivery.
func (lrw *loggingResponseWriter) Flush() {
	if f, ok := lrw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
