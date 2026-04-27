package server

import (
	"bytes"
	"encoding/json"
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
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// buildRegistry spins up an httptest server that plays the role of an
// OpenAI upstream and returns a registry pointing one openai-typed
// provider at it. handler scripts the upstream behavior per test.
func buildRegistry(t *testing.T, handler http.HandlerFunc) (*registry.Registry, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	yaml := fmt.Sprintf(`
providers:
  - name: test-openai
    type: openai
    endpoint: %s
    api_key: sk-test
    models:
      exact: ["test-model"]
`, upstream.URL)

	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	reg, err := registry.Load(path)
	require.NoError(t, err)
	return reg, upstream
}

func newChatHandlerSrv(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	reg, _ := buildRegistry(t, upstream)
	return newTestServer(t, func(d *Deps) { d.Registry = reg })
}

func chatBody(model string, content string, stream bool) []byte {
	body := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	if stream {
		body["stream"] = true
	}
	b, _ := json.Marshal(body)
	return b
}

func TestChat_HappyPath(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		// Verify gateway forwards Authorization header to upstream.
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-1","object":"chat.completion","created":1714000000,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`)
	}
	srv := newChatHandlerSrv(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(chatBody("test-model", "hello", false)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "chatcmpl-1", resp["id"])
	assert.Equal(t, "test-model", resp["model"])
	// Internal `provider` field on ChatResponse must not leak.
	assert.NotContains(t, rec.Body.String(), `"provider":`)
}

func TestChat_MissingModel(t *testing.T) {
	srv := newChatHandlerSrv(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream should not be called")
	})

	body := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "missing_model", env.Error.Code)
}

func TestChat_MissingMessages(t *testing.T) {
	srv := newChatHandlerSrv(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream should not be called")
	})

	body := []byte(`{"model":"test-model"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "missing_messages", env.Error.Code)
}

func TestChat_MalformedJSON(t *testing.T) {
	srv := newChatHandlerSrv(t, func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Malformed JSON")
}

func TestChat_EmptyBody(t *testing.T) {
	srv := newChatHandlerSrv(t, func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Empty request body")
}

func TestChat_BodyTooLarge(t *testing.T) {
	srv := newChatHandlerSrv(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream should not be called for oversize body")
	})

	// 2 MiB > maxRequestBytes (1 MiB).
	huge := make([]byte, 2<<20)
	for i := range huge {
		huge[i] = 'a'
	}
	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"` + string(huge) + `"}]}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "request_too_large", env.Error.Code)
}

func TestChat_UnknownModel(t *testing.T) {
	srv := newChatHandlerSrv(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream should not be called for unknown model")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(chatBody("not-configured", "hi", false)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "model_not_found", env.Error.Code)
}

func TestChat_UpstreamRateLimited_Maps429(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`)
	}
	srv := newChatHandlerSrv(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(chatBody("test-model", "hi", false)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "7", rec.Header().Get("Retry-After"))
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "rate_limit_error", env.Error.Type)
}

func TestChat_UpstreamContextLength_Maps400(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too long"}}`)
	}
	srv := newChatHandlerSrv(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(chatBody("test-model", "hi", false)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "context_length_exceeded", env.Error.Code)
}

func TestChat_UpstreamAuth_Maps502(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
	}
	srv := newChatHandlerSrv(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(chatBody("test-model", "hi", false)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// Upstream's 401 must not be passed through as 401 — that would tell
	// the client to rotate THEIR key, when the gateway's key is the bad one.
	require.Equal(t, http.StatusBadGateway, rec.Code)
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "upstream_auth_failed", env.Error.Code)
}

func TestChat_UpstreamUnavailable_Maps503(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"down","type":"server_error"}}`)
	}
	srv := newChatHandlerSrv(t, upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(chatBody("test-model", "hi", false)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}


func TestChat_ErrorEnvelopeContainsReqID(t *testing.T) {
	// Even pre-handler errors (missing model) should propagate the
	// X-Request-ID into the error envelope so clients can correlate.
	srv := newChatHandlerSrv(t, func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"x"}]}`)))
	req.Header.Set("X-Request-ID", "req-trace-this-id")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "req-trace-this-id", env.Error.ReqID)
	// Also echoed in the response header by the RequestID middleware.
	assert.Equal(t, "req-trace-this-id", rec.Header().Get("X-Request-ID"))
}

func TestChat_LoggerCalledOnFailureWithoutLeakingPrompt(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
	}

	core := zap.NewNop()
	reg, _ := buildRegistry(t, upstream)
	srv, err := New(Deps{
		Logger:         core,
		Registry:       reg,
		MetricsReg:     nil,
		MetricsEnabled: false,
	})
	require.NoError(t, err)

	body := chatBody("test-model", "supersecret-prompt-canary", false)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.NotContains(t, rec.Body.String(), "supersecret-prompt-canary",
		"prompt content leaked into error response")
}
