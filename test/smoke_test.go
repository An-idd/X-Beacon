// Package smoke holds black-box integration tests that exercise the
// gateway as a single piece — registry + auth + middleware chain +
// chat handlers — against httptest-mocked upstreams for all three
// providers (OpenAI, Anthropic, DeepSeek).
//
// These tests live outside internal/ so they can only use the public API
// surface that real consumers of the gateway would.
package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/router"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/server"
	"github.com/An-idd/x-beacon/pkg/version"
)

// fixture builds a fully-wired Server backed by three httptest upstreams
// (one per provider type) plus a static auth table. Each upstream's
// handler is supplied per-test so behavior can be scripted.
type fixture struct {
	gateway  *httptest.Server
	openai   *httptest.Server
	anthrop  *httptest.Server
	deepseek *httptest.Server

	// AuthKey is the bearer token configured in the static auth table.
	AuthKey string

	// dbReady / redisReady drive the /readyz checkers. Tests flip them
	// to simulate dependency health transitions without rebuilding the
	// fixture; both default to true on construction.
	dbReady    *atomic.Bool
	redisReady *atomic.Bool
}

func (f *fixture) Close() {
	f.gateway.Close()
	f.openai.Close()
	f.anthrop.Close()
	f.deepseek.Close()
}

func newFixture(
	t *testing.T,
	openaiHandler http.HandlerFunc,
	anthropicHandler http.HandlerFunc,
	deepseekHandler http.HandlerFunc,
) *fixture {
	t.Helper()
	openai := httptest.NewServer(openaiHandler)
	anthrop := httptest.NewServer(anthropicHandler)
	deepseek := httptest.NewServer(deepseekHandler)

	yaml := fmt.Sprintf(`
providers:
  - name: openai-mock
    type: openai
    endpoint: %s
    api_key: sk-openai-mock
    models:
      exact: ["gpt-4o", "gpt-4o-mini"]
  - name: anthropic-mock
    type: anthropic
    endpoint: %s
    api_key: sk-anthropic-mock
    models:
      exact: ["claude-3-5-sonnet"]
  - name: deepseek-mock
    type: deepseek
    endpoint: %s
    api_key: sk-deepseek-mock
    models:
      exact: ["deepseek-chat", "deepseek-reasoner"]
`, openai.URL, anthrop.URL, deepseek.URL)

	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(yamlPath, []byte(yaml), 0o600))

	reg, err := registry.Load(yamlPath)
	require.NoError(t, err)

	const authKey = "sk-gateway-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "smoke", Name: "Smoke test", Secret: authKey},
	})
	require.NoError(t, err)

	dbReady := &atomic.Bool{}
	dbReady.Store(true)
	redisReady := &atomic.Bool{}
	redisReady.Store(true)

	// Two readiness checkers parameterized by the controllable flags below.
	// Tests can flip dbReady / redisReady between calls to simulate
	// dependency health changes without rebuilding the whole fixture.
	checkers := []server.ReadinessChecker{
		{Name: "postgres", Check: func(context.Context) error {
			if !dbReady.Load() {
				return errors.New("postgres unreachable (test)")
			}
			return nil
		}},
		{Name: "redis", Check: func(context.Context) error {
			if !redisReady.Load() {
				return errors.New("redis unreachable (test)")
			}
			return nil
		}},
	}

	rtr := router.New(reg, router.DefaultPolicy(), zap.NewNop())

	srv, err := server.New(server.Deps{
		Logger:            zap.NewNop(),
		Registry:          reg,
		Router:            rtr,
		Authn:             authn,
		MetricsReg:        prometheus.NewRegistry(),
		MetricsEnabled:    true,
		MetricsPath:       "/metrics",
		ReadinessCheckers: checkers,
	})
	require.NoError(t, err)

	gateway := httptest.NewServer(srv.Handler())

	return &fixture{
		gateway:    gateway,
		openai:     openai,
		anthrop:    anthrop,
		deepseek:   deepseek,
		AuthKey:    authKey,
		dbReady:    dbReady,
		redisReady: redisReady,
	}
}

// post issues an authenticated POST and returns status, headers, body.
func (f *fixture) post(t *testing.T, path string, body []byte, withAuth bool) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.gateway.URL+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+f.AuthKey)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, raw
}

func (f *fixture) get(t *testing.T, path string, withAuth bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.gateway.URL+path, nil)
	require.NoError(t, err)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+f.AuthKey)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// --- Mock upstream payloads -------------------------------------------------

func openaiChatResponse() string {
	return `{
		"id":"chatcmpl-1","object":"chat.completion","created":1714000000,"model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi from openai"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":3,"completion_tokens":3,"total_tokens":6}
	}`
}

func anthropicChatResponse() string {
	// Anthropic's /v1/messages response shape (NOT the OpenAI shape).
	return `{
		"id":"msg_1","type":"message","role":"assistant",
		"content":[{"type":"text","text":"hi from anthropic"}],
		"model":"claude-3-5-sonnet","stop_reason":"end_turn",
		"usage":{"input_tokens":5,"output_tokens":4}
	}`
}

func deepseekChatResponse() string {
	return `{
		"id":"chatcmpl-ds-1","object":"chat.completion","created":1714000000,"model":"deepseek-chat",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi from deepseek"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":3,"completion_tokens":3,"total_tokens":6}
	}`
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, err := io.WriteString(w, body)
	require.NoError(t, err)
}

// chatRequest builds a minimal OpenAI-shape request body.
func chatRequest(model string, stream bool) []byte {
	body := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	if stream {
		body["stream"] = true
	}
	b, _ := json.Marshal(body)
	return b
}

// --- Tests ------------------------------------------------------------------

func TestSmoke_ModelsReturnsAllProviders(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, raw := fx.get(t, "/v1/models", true)
	require.Equal(t, http.StatusOK, status)

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &resp))
	assert.Equal(t, "list", resp.Object)
	assert.Len(t, resp.Data, 5, "expect 2 openai + 1 anthropic + 2 deepseek")

	owners := map[string]int{}
	for _, m := range resp.Data {
		owners[m.OwnedBy]++
	}
	assert.Equal(t, 2, owners["openai"])
	assert.Equal(t, 1, owners["anthropic"])
	assert.Equal(t, 2, owners["deepseek"])
}

func TestSmoke_AuthIsEnforced(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	// Without auth → 401
	status, _ := fx.get(t, "/v1/models", false)
	assert.Equal(t, http.StatusUnauthorized, status)

	// /healthz still public
	statusH, _ := fx.get(t, "/healthz", false)
	assert.Equal(t, http.StatusOK, statusH)
}

func TestSmoke_NonStreaming_OpenAI(t *testing.T) {
	fx := newFixture(t,
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, openaiChatResponse()) },
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, _, raw := fx.post(t, "/v1/chat/completions", chatRequest("gpt-4o", false), true)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(raw), "hi from openai")
}

func TestSmoke_NonStreaming_Anthropic(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, anthropicChatResponse()) },
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, _, raw := fx.post(t, "/v1/chat/completions", chatRequest("claude-3-5-sonnet", false), true)
	require.Equal(t, http.StatusOK, status)
	// Anthropic adapter converts to OpenAI shape — content should surface
	// inside choices[0].message.content even though upstream sent
	// content_blocks.
	assert.Contains(t, string(raw), "hi from anthropic")
	assert.Contains(t, string(raw), `"finish_reason":"stop"`)
}

func TestSmoke_NonStreaming_DeepSeek(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, deepseekChatResponse()) },
	)
	defer fx.Close()

	status, _, raw := fx.post(t, "/v1/chat/completions", chatRequest("deepseek-chat", false), true)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(raw), "hi from deepseek")
}

func TestSmoke_Streaming_OpenAI(t *testing.T) {
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"streamed"}}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	fx := newFixture(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, f := range frames {
				_, _ = io.WriteString(w, f)
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
		},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, hdr, raw := fx.post(t, "/v1/chat/completions", chatRequest("gpt-4o", true), true)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "text/event-stream", hdr.Get("Content-Type"))

	body := string(raw)
	assert.Contains(t, body, `"role":"assistant"`)
	assert.Contains(t, body, `"content":"streamed"`)
	assert.Contains(t, body, `"finish_reason":"stop"`)
	assert.True(t, strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]"),
		"stream should terminate with [DONE]; got body=%q", body)
}

func TestSmoke_Streaming_Anthropic(t *testing.T) {
	// Anthropic streaming uses the `event: type` + JSON form with multiple
	// event kinds. The adapter translates to OpenAI-shape chunks.
	frames := []string{
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-3-5-sonnet","usage":{"input_tokens":5}}}` + "\n\n",
		"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streamed-anthropic"}}` + "\n\n",
		"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n",
		"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	}
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, f := range frames {
				_, _ = io.WriteString(w, f)
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
		},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, _, raw := fx.post(t, "/v1/chat/completions", chatRequest("claude-3-5-sonnet", true), true)
	require.Equal(t, http.StatusOK, status)
	body := string(raw)
	assert.Contains(t, body, "streamed-anthropic")
	assert.Contains(t, body, `"finish_reason":"stop"`) // mapped from end_turn
	assert.True(t, strings.Contains(body, "data: [DONE]"))
}

func TestSmoke_UnknownModelReturns400(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, _, raw := fx.post(t, "/v1/chat/completions", chatRequest("not-configured", false), true)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, string(raw), "model_not_found")
}

func TestSmoke_Readyz_Healthy(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	// Both deps default to ready.
	status, raw := fx.get(t, "/readyz", false) // /readyz must work without auth
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(raw), `"ready":true`)
}

func TestSmoke_Readyz_DBDown(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	fx.dbReady.Store(false)

	status, raw := fx.get(t, "/readyz", false)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	body := string(raw)
	assert.Contains(t, body, `"ready":false`)
	assert.Contains(t, body, "postgres unreachable")
	// Redis still OK so its block must reflect that.
	assert.Contains(t, body, `"redis":{"ok":true}`)
}

// TestSmoke_FailoverFromOpenAIToDeepseek verifies the Week 6 router's
// fail-over wiring at the HTTP layer. Two openai-compatible providers
// claim the same model: openai-mock as exact owner (primary) and
// deepseek-fallback via glob (declared after, so it sits second in the
// chain). When the OpenAI mock returns 503 on every call the router
// exhausts retries, fails over to the deepseek mock, and the gateway
// surfaces the second provider's response.
func TestSmoke_FailoverFromOpenAIToDeepseek(t *testing.T) {
	openaiCalls := atomic.Int32{}
	openaiHandler := func(w http.ResponseWriter, _ *http.Request) {
		openaiCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream down","type":"server_error"}}`)
	}
	deepseekCalls := atomic.Int32{}
	deepseekHandler := func(w http.ResponseWriter, _ *http.Request) {
		deepseekCalls.Add(1)
		writeJSON(t, w, `{
			"id":"chatcmpl-failover","object":"chat.completion","created":1714000000,"model":"gpt-4o",
			"choices":[{"index":0,"message":{"role":"assistant","content":"served-by-fallback"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}

	openai := httptest.NewServer(http.HandlerFunc(openaiHandler))
	deepseek := httptest.NewServer(http.HandlerFunc(deepseekHandler))
	defer openai.Close()
	defer deepseek.Close()

	yaml := fmt.Sprintf(`
providers:
  - name: openai-mock
    type: openai
    endpoint: %s
    api_key: sk-openai-mock
    models:
      exact: ["gpt-4o"]
  - name: deepseek-fallback
    type: deepseek
    endpoint: %s
    api_key: sk-deepseek-mock
    models:
      glob: ["gpt-*"]
`, openai.URL, deepseek.URL)

	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(yamlPath, []byte(yaml), 0o600))
	reg, err := registry.Load(yamlPath)
	require.NoError(t, err)

	const authKey = "sk-failover-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{{ID: "fo", Name: "Failover", Secret: authKey}})
	require.NoError(t, err)

	metricsReg := prometheus.NewRegistry()
	metrics, err := observability.NewMetrics(metricsReg)
	require.NoError(t, err)
	rtr := router.New(reg, router.DefaultPolicy(), zap.NewNop(), router.WithMetrics(metrics))
	srv, err := server.New(server.Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         rtr,
		Authn:          authn,
		Metrics:        metrics,
		MetricsReg:     metricsReg,
		MetricsEnabled: true,
		MetricsPath:    "/metrics",
	})
	require.NoError(t, err)
	gateway := httptest.NewServer(srv.Handler())
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions",
		bytes.NewReader(chatRequest("gpt-4o", false)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+authKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", body)
	assert.Contains(t, string(body), "served-by-fallback")
	assert.GreaterOrEqual(t, openaiCalls.Load(), int32(1), "openai must have been tried at least once")
	assert.EqualValues(t, 1, deepseekCalls.Load(), "deepseek (fallback) must have served exactly once")

	// M2 acceptance: failover must surface as a metric increment, not just
	// a log line. Scrape /metrics and assert the counter fired at least
	// once for the openai-mock → deepseek-fallback hop.
	scrapeReq, _ := http.NewRequest(http.MethodGet, gateway.URL+"/metrics", nil)
	scrapeResp, err := http.DefaultClient.Do(scrapeReq)
	require.NoError(t, err)
	defer scrapeResp.Body.Close()
	scrape, _ := io.ReadAll(scrapeResp.Body)
	assert.Contains(t, string(scrape),
		`gateway_router_failover_total{from="openai-mock",to="deepseek-fallback"}`,
		"failover counter must surface in /metrics")
}

func TestSmoke_UserAgentForwarded(t *testing.T) {
	// Capture the user-agent the gateway sends to the upstream and verify
	// the pkg/version-injected value is passed through. This guards against
	// future refactors silently dropping the header.
	var seenUA atomic.Value // string
	openaiHandler := func(w http.ResponseWriter, r *http.Request) {
		seenUA.Store(r.Header.Get("User-Agent"))
		writeJSON(t, w, openaiChatResponse())
	}
	fx := newFixture(t,
		openaiHandler,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, _, _ := fx.post(t, "/v1/chat/completions", chatRequest("gpt-4o", false), true)
	require.Equal(t, http.StatusOK, status)

	got, _ := seenUA.Load().(string)
	assert.Equal(t, version.UserAgent(), got, "gateway must forward pkg/version UA verbatim")
	assert.True(t, strings.HasPrefix(got, "x-beacon/"), "got %q", got)
}

