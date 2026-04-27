package middleware

import (
	"net/http"
	"runtime/debug"

	"go.uber.org/zap"
)

// Recovery is middleware that traps panics from the handler chain below.
// On panic it writes a 500 JSON error (if the response hasn't started),
// logs the panic value + stack trace + req_id at error level, and never
// re-raises — keeping the server up.
//
// Recovery is mounted as the outermost middleware so it covers every other
// middleware as well as the handler itself.
func Recovery(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented "I'm bailing out
				// silently" signal; respect it (no log, no 500).
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				logger.Error("panic recovered",
					zap.Any("panic", rec),
					zap.String("req_id", RequestIDFrom(r.Context())),
					zap.String("method", r.Method),
					zap.String("path", r.URL.Path),
					zap.ByteString("stack", debug.Stack()),
				)

				// Best-effort 500. If headers are already flushed (e.g. SSE),
				// WriteHeader is a no-op — that's fine, the client already saw
				// some output and will detect the truncation.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"internal server error","type":"internal_error"}}` + "\n"))
			}()
			next.ServeHTTP(w, r)
		})
	}
}
