package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// HeaderRequestID is the HTTP header carrying the request ID, both inbound
// (caller-supplied for trace continuity) and outbound (echoed so the caller
// can correlate with their logs).
const HeaderRequestID = "X-Request-ID"

// requestIDKey is the unexported context key under which the ID lives.
// Using a typed empty struct prevents collisions with other packages.
type requestIDKey struct{}

// RequestID is middleware that ensures every request has an ID. If the
// inbound request carries X-Request-ID, that value is reused; otherwise a
// UUIDv4 is generated. The ID is injected into the request context via
// WithRequestID and echoed in the response header.
//
// Inbound IDs are accepted as-is up to maxInboundLen runes; longer values
// are discarded and replaced with a fresh UUID. This avoids unbounded log
// fields if an upstream sends garbage. No content validation beyond length —
// trace systems set their own format.
func RequestID() func(http.Handler) http.Handler {
	const maxInboundLen = 128
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if id == "" || len(id) > maxInboundLen {
				id = uuid.NewString()
			}
			w.Header().Set(HeaderRequestID, id)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDFrom returns the ID stored on ctx by the RequestID middleware,
// or the empty string if none is present.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}
