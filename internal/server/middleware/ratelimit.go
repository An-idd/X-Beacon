package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/ratelimit"
)

// RateLimit is middleware that runs every configured rule once per
// request; the first deny short-circuits with 429 + Retry-After.
//
// Mounting position (relative to other middleware):
//
//	Recovery → RequestID → Tracing → Logging → /v1/{ Auth → RateLimit → handler }
//
// After Auth is critical because the most useful rate keys (api_key,
// model) come from the Principal + request body. RateLimit handles
// missing Principal gracefully (counts toward "anonymous" buckets).
//
// Streaming requests are limited only at the entry — once a stream
// starts (handler returns the channel), no further rate checks fire.
// This matches OpenAI's observed behavior; mid-stream rejection would
// truncate output and confuse SDKs.
//
// Backend errors (Redis outage etc.) **fail open** — request passes
// through, error is logged at warn. Failing closed during a Redis
// outage would amount to "Redis tickle = full outage", which is worse
// than the temporary loss of enforcement.
func RateLimit(multi *ratelimit.Multi, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// nil/empty multi → no-op pass-through; cheaper than checking on
		// every request.
		if multi == nil || multi.Len() == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			kctx := buildKeyContext(r)
			d, err := multi.Check(r.Context(), kctx, 1)
			if err != nil {
				logger.Warn("ratelimit backend error; failing open",
					zap.String("req_id", RequestIDFrom(r.Context())),
					zap.Error(err))
				next.ServeHTTP(w, r)
				return
			}

			writeRateLimitHeaders(w, d)

			if !d.Allowed {
				logger.Info("ratelimit hit",
					zap.String("req_id", RequestIDFrom(r.Context())),
					zap.String("rule", d.Rule),
					zap.String("path", r.URL.Path),
					zap.Int("limit", d.Limit))
				writeRateLimitDenied(w, d, RequestIDFrom(r.Context()))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// buildKeyContext derives the per-request dimensions from the request.
// Principal comes from the auth middleware (already run); model comes
// from the request body but parsing it here would require buffering —
// so middleware leaves model="" and rate-limit rules using KeyByModel
// effectively become per-API-key (best-effort). The chat handler can
// re-issue a fine-grained check post-parse if Week 5+ requires it.
func buildKeyContext(r *http.Request) ratelimit.KeyContext {
	kctx := ratelimit.KeyContext{}
	if p := auth.PrincipalFrom(r.Context()); p != nil {
		kctx.APIKeyID = p.ID
	}
	return kctx
}

// writeRateLimitHeaders attaches the standard X-RateLimit-* trio so
// clients can self-pace. Headers are set on every outcome (Allowed too)
// because they're informational, not error-only.
func writeRateLimitHeaders(w http.ResponseWriter, d ratelimit.Decision) {
	if d.Limit > 0 {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
	}
	if !d.Reset.IsZero() {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(d.Reset.Unix(), 10))
	}
}

// writeRateLimitDenied serializes the OpenAI-shaped 429 envelope.
func writeRateLimitDenied(w http.ResponseWriter, d ratelimit.Decision, reqID string) {
	if d.RetryAfter > 0 {
		// Round up to whole seconds; HTTP Retry-After is integer-only.
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(d.RetryAfter)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)

	// Mirrors the OpenAI rate_limit_error shape so existing clients
	// surface a useful message without adapter logic.
	body := map[string]any{
		"error": map[string]any{
			"type":    "rate_limit_error",
			"code":    "rate_limit_exceeded",
			"message": fmt.Sprintf("Rate limit exceeded for rule %q. Retry after %s.", d.Rule, d.RetryAfter.Round(time.Second)),
			"req_id":  reqID,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

// retryAfterSeconds rounds up so we never under-promise. A sub-second
// wait still prompts the client to wait at least 1 second, which is
// kinder to backends than a tight retry loop.
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	s := int(d / time.Second)
	if d%time.Second > 0 {
		s++
	}
	if s < 1 {
		s = 1
	}
	return s
}

