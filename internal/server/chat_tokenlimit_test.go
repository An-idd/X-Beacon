package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/ratelimit"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
)

// tokenRuleMulti builds a Multi with one token-unit rule with the given
// per-minute budget (burst == budget so the first over-budget prompt is
// the one denied).
func tokenRuleMulti(t *testing.T, budget int) *ratelimit.Multi {
	t.Helper()
	rules, err := ratelimit.Build([]ratelimit.RuleConfig{{
		Name:      "tpm",
		Algorithm: "memory_bucket",
		Rate:      "60/m", // 1 token/s refill — negligible within one test
		Burst:     budget,
		Unit:      "tokens",
		KeyBy:     []string{"api_key"},
	}}, nil)
	require.NoError(t, err)
	return ratelimit.NewMulti(rules...)
}

func TestChat_TokenRateLimit_DeniesWhenBudgetExhausted(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"test-model",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	reg, _ := buildRegistry(t, upstream)
	tk, err := tokenizer.NewSelector()
	require.NoError(t, err)
	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Tokenizer = tk
		d.RateLimiter = tokenRuleMulti(t, 30) // ~30-token budget
	})

	post := func(content string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader(chatBody("test-model", content, false)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Small prompt fits the 30-token budget.
	rec := post("hi")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Second request's prompt pushes past the remaining budget → 429
	// with the OpenAI rate_limit envelope and a Retry-After header.
	long := strings.Repeat("exceedingly verbose padding sentence to inflate the token count. ", 12)
	rec = post(long)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "rate_limit_error")
	assert.Contains(t, rec.Body.String(), "tpm")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
}

func TestChat_TokenRateLimit_RequestRulesUnaffected(t *testing.T) {
	// A token-unit rule must not consume request-unit credits: with only
	// a token rule configured, many small requests all pass.
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"test-model",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	reg, _ := buildRegistry(t, upstream)
	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.RateLimiter = tokenRuleMulti(t, 1000)
	})

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader(chatBody("test-model", "hi", false)))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "request %d body: %s", i, rec.Body.String())
	}
}
