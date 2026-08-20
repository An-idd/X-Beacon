package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// buildAnthropicRegistry builds a registry with one anthropic-type
// provider pointing at the given upstream handler.
func buildAnthropicRegistry(t *testing.T, upstream http.HandlerFunc) (*registry.Registry, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	yaml := fmt.Sprintf(`
providers:
  - name: test-anthropic
    type: anthropic
    endpoint: %s
    api_key: sk-ant-test
    models:
      exact: ["claude-test"]
      glob: ["claude-*"]
`, srv.URL)
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	reg, err := registry.Load(path)
	require.NoError(t, err)
	return reg, srv
}

func newMessagesSrv(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	reg, _ := buildAnthropicRegistry(t, upstream)
	return newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
	})
}

func TestMessages_NonStreaming_PassthroughWithThinking(t *testing.T) {
	// The response contains a thinking block — a structure the gateway
	// does not model. It must reach the client byte-faithful.
	upstreamResp := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-test",` +
		`"content":[{"type":"thinking","thinking":"let me reason...","signature":"sig_abc"},` +
		`{"type":"text","text":"hello"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":25,"output_tokens":10,"cache_read_input_tokens":5}}`

	var gotBody []byte
	var gotBeta string
	srv := newMessagesSrv(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamResp)
	})

	reqBody := `{"model":"claude-test","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.JSONEq(t, upstreamResp, rec.Body.String(), "response must pass through verbatim, thinking block intact")
	assert.JSONEq(t, reqBody, string(gotBody), "request must pass through verbatim")
	assert.Equal(t, "prompt-caching-2024-07-31", gotBeta)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

func TestMessages_Streaming_ThinkingDeltasVerbatim(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_02\",\"usage\":{\"input_tokens\":30,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step 1...\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_xyz\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":12}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	srv := newMessagesSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	})

	reqBody := `{"model":"claude-test","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	ct := strings.Split(rec.Header().Get("Content-Type"), ";")[0]
	assert.Equal(t, "text/event-stream", strings.TrimSpace(ct))
	// Byte-for-byte: Anthropic SSE uses event: lines; any re-framing
	// breaks the official SDK's typed event parser.
	assert.Equal(t, strings.Join(frames, ""), rec.Body.String(),
		"SSE frames (incl. thinking_delta / signature_delta) must pass through verbatim")
}

func TestMessages_UpstreamErrorEnvelopePassthrough(t *testing.T) {
	// Upstream errors are Anthropic-shaped already; forward status +
	// envelope untouched so SDK error classes map correctly.
	upstreamErr := `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`
	srv := newMessagesSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, upstreamErr)
	})

	reqBody := `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.JSONEq(t, upstreamErr, rec.Body.String())
	assert.Equal(t, "7", rec.Header().Get("Retry-After"), "Retry-After must pass through")
}

func TestMessages_ModelNotServedByAnthropic_404(t *testing.T) {
	// Model resolves to an OpenAI-type provider (or nothing): the native
	// endpoint must return an Anthropic-shaped error, not an OpenAI one.
	srv := newMessagesSrv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called")
	})

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"type":"error"`)
	assert.Contains(t, body, `"not_found_error"`)
	assert.NotContains(t, body, `"error":{"type":"invalid_request_error","code"`,
		"must be Anthropic error shape, not OpenAI envelope")
}

func TestMessages_MalformedJSON_400(t *testing.T) {
	srv := newMessagesSrv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"invalid_request_error"`)
}

func TestPipeAnthropicSSE_ExtractsUsageWithoutMutation(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":42,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}` + "\n\n"

	rec := httptest.NewRecorder()
	usage := pipeAnthropicSSE(rec, strings.NewReader(stream))

	assert.Equal(t, 42, usage.InputTokens, "input_tokens from message_start")
	assert.Equal(t, 17, usage.OutputTokens, "output_tokens from message_delta")
	assert.Equal(t, stream, rec.Body.String(), "scan must not mutate the piped bytes")
}
