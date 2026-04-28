package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/ratelimit"
)

// stubLimiter scripts a Decision (or error) for any key. Used so the
// middleware tests don't depend on the real memory/redis backends.
type stubLimiter struct {
	resp Decision
	err  error
}

// Decision is a local alias to avoid a cross-package generic type
// helper; ratelimit.Decision is the actual type.
type Decision = ratelimit.Decision

func (s *stubLimiter) Allow(_ context.Context, _ string, _ int) (ratelimit.Decision, error) {
	return s.resp, s.err
}

func newMulti(rules ...*stubLimiter) *ratelimit.Multi {
	wrapped := make([]*ratelimit.Rule, 0, len(rules))
	for i, r := range rules {
		wrapped = append(wrapped, &ratelimit.Rule{
			Name:    "rule-" + strconv.Itoa(i),
			Limiter: r,
		})
	}
	return ratelimit.NewMulti(wrapped...)
}

// chain wraps the handler with RequestID + RateLimit so tests can assert
// the X-Request-ID context plumbing along with rate-limit behavior.
func chain(authnPrincipal *auth.Principal, h http.Handler, m *ratelimit.Multi) http.Handler {
	rl := RateLimit(m, nil, zap.NewNop())(h)
	if authnPrincipal != nil {
		// Inject Principal so api_key-keyed rules see it.
		injected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.WithPrincipal(r.Context(), authnPrincipal)
			rl.ServeHTTP(w, r.WithContext(ctx))
		})
		return RequestID()(injected)
	}
	return RequestID()(rl)
}

func TestRateLimit_NilMulti_PassThrough(t *testing.T) {
	called := false
	h := chain(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	assert.True(t, called, "nil multi must be a no-op")
}

func TestRateLimit_EmptyMulti_PassThrough(t *testing.T) {
	called := false
	h := chain(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), ratelimit.NewMulti())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	assert.True(t, called)
}

func TestRateLimit_AllowSetsHeaders(t *testing.T) {
	reset := time.Date(2026, 4, 27, 13, 0, 0, 0, time.UTC)
	m := newMulti(&stubLimiter{resp: Decision{
		Allowed: true, Limit: 100, Remaining: 42, Reset: reset,
	}})
	h := chain(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), m)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "100", rec.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "42", rec.Header().Get("X-RateLimit-Remaining"))
	assert.Equal(t, strconv.FormatInt(reset.Unix(), 10), rec.Header().Get("X-RateLimit-Reset"))
}

func TestRateLimit_DenyReturns429(t *testing.T) {
	m := newMulti(&stubLimiter{resp: Decision{
		Allowed: false, Limit: 60, Remaining: 0,
		RetryAfter: 7 * time.Second,
	}})
	h := chain(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run after deny")
	}), m)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "7", rec.Header().Get("Retry-After"))
	assert.Equal(t, "60", rec.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "0", rec.Header().Get("X-RateLimit-Remaining"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errBody := body["error"].(map[string]any)
	assert.Equal(t, "rate_limit_error", errBody["type"])
	assert.Equal(t, "rate_limit_exceeded", errBody["code"])
	assert.NotEmpty(t, errBody["req_id"])
}

func TestRateLimit_RetryAfterRoundsUpSubSecond(t *testing.T) {
	m := newMulti(&stubLimiter{resp: Decision{
		Allowed: false, Limit: 10, RetryAfter: 250 * time.Millisecond,
	}})
	h := chain(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), m)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	assert.Equal(t, "1", rec.Header().Get("Retry-After"),
		"sub-second retry must round up to 1s, never 0")
}

func TestRateLimit_BackendError_FailsOpen(t *testing.T) {
	m := newMulti(&stubLimiter{err: errors.New("redis: outage")})
	called := false
	h := chain(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), m)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	assert.True(t, called, "backend error must fail open, not block traffic")
	assert.NotEqual(t, http.StatusTooManyRequests, rec.Code)
}

func TestRateLimit_PrincipalKey(t *testing.T) {
	// Verify the middleware actually plucks Principal from ctx so
	// api_key-keyed rules see the right tenant. The stub doesn't
	// inspect the key, so we infer correctness via a Rule wired with
	// KeyByAPIKey and a fakeLimiter that records keys.
	type recordingLimiter struct {
		seen []string
	}
	rec := &recordingLimiter{}

	rl := &recordingLimiterImpl{seen: &rec.seen}
	rule := &ratelimit.Rule{
		Name:    "per-key",
		KeyBy:   []ratelimit.KeyBy{ratelimit.KeyByAPIKey},
		Limiter: rl,
	}
	multi := ratelimit.NewMulti(rule)

	h := chain(&auth.Principal{ID: "k7"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), multi)

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/x", nil))

	require.Len(t, rec.seen, 1)
	assert.Contains(t, rec.seen[0], "k7", "composed key must include principal ID")
}

// recordingLimiterImpl records the key it sees and always allows.
type recordingLimiterImpl struct{ seen *[]string }

func (r *recordingLimiterImpl) Allow(_ context.Context, key string, _ int) (ratelimit.Decision, error) {
	*r.seen = append(*r.seen, key)
	return Decision{Allowed: true, Limit: 1000, Remaining: 999}, nil
}

func TestRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 0},
		{500 * time.Millisecond, 1},
		{1 * time.Second, 1},
		{1500 * time.Millisecond, 2},
		{30 * time.Second, 30},
		{-1 * time.Second, 0},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, retryAfterSeconds(c.in), "in=%v", c.in)
	}
}
