package middleware

import (
	"net/http"
	"strconv"
)

// CORS returns middleware that handles cross-origin requests for /admin/*
// against an explicit origin allowlist. Wildcards (`*`, `*.example.com`)
// are NOT supported by design — the admin surface is small enough to list
// origins explicitly, and wildcard policies historically leak.
//
// Mount BEFORE Auth so preflight (OPTIONS) requests, which browsers send
// without Authorization, can complete the handshake. Other request methods
// still flow through Auth + RequireScope downstream.
//
// When allowedOrigins is empty (default), this middleware is a transparent
// no-op — no CORS headers are emitted, browsers reject any cross-origin
// access. That keeps the deployment safe-by-default; ops must opt in to
// each WebUI host.
//
// Headers emitted on a matching origin:
//
//	Access-Control-Allow-Origin: <echoed>
//	Vary: Origin
//	Access-Control-Allow-Credentials: true (so the browser sends Authorization)
//	Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
//	Access-Control-Allow-Headers: Authorization, Content-Type
//	Access-Control-Max-Age: 600 (preflight cache)
//
// On a non-matching origin, NO CORS headers are emitted. The browser's
// own SOP enforcement then refuses the response.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	// Build a set for O(1) lookup. Empty input means "no CORS at all".
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o != "" {
			allowed[o] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Always set Vary so caches don't poison cross-origin
			// responses with the wrong CORS headers. Cheap, correct,
			// recommended even when no origin matched.
			if origin != "" {
				w.Header().Add("Vary", "Origin")
			}

			if _, ok := allowed[origin]; ok && origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", strconv.Itoa(corsMaxAgeSeconds))
			}

			// Preflight: succeed without Authorization. Even if origin
			// didn't match, returning 204 lets the browser's SOP do
			// the actual rejection (no Allow-Origin header echoed).
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// corsMaxAgeSeconds caches the preflight response in browsers; 10 min
// is the sweet spot — long enough that a busy WebUI session doesn't
// re-preflight on every navigation, short enough that allowlist
// changes propagate quickly after a config reload + browser refresh.
const corsMaxAgeSeconds = 600
