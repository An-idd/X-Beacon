package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/prompt"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
)

// upstreamCapture records the last request body the upstream saw, so
// the test can assert which messages the gateway actually forwarded
// after compression.
type upstreamCapture struct {
	lastBody []byte
}

func (u *upstreamCapture) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		u.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-c","object":"chat.completion","created":1,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`)
	}
}

func TestChat_PromptCompressed_DropsOldMessages(t *testing.T) {
	cap := &upstreamCapture{}
	srv := newChatHandlerSrv(t, cap.handler(t))

	tk, err := tokenizer.NewSelector()
	require.NoError(t, err)

	// Tiny budget to force compression. min_keep=1 means at least
	// one non-system message survives.
	srv.deps.Compressor = prompt.NewSlidingWindow(prompt.SlidingWindowOptions{
		Tokenizer:       tk,
		DefaultWindow:   100,
		TriggerRatio:    0.5,
		MinKeepMessages: 1,
	})
	// Re-mount routes so the handler closes over the new Compressor.
	srv2, err := New(srv.deps)
	require.NoError(t, err)

	pad := strings.Repeat("word ", 80)
	body := map[string]any{
		"model": "test-model",
		"messages": []map[string]any{
			{"role": "system", "content": "be concise"},
			{"role": "user", "content": pad}, // dropped
			{"role": "assistant", "content": pad}, // dropped
			{"role": "user", "content": "tail"},   // kept (most recent)
		},
	}
	raw, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("X-X-Beacon-Prompt-Compressed"),
		"compressed header should be set when messages were dropped")

	// Decode upstream-observed payload to confirm trimming happened
	// on the wire, not just internally.
	var fwd struct {
		Messages []map[string]string `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(cap.lastBody, &fwd))
	assert.Len(t, fwd.Messages, 2, "expected system + tail-user only; got %d", len(fwd.Messages))
	assert.Equal(t, "system", fwd.Messages[0]["role"])
	assert.Equal(t, "user", fwd.Messages[1]["role"])
	assert.Equal(t, "tail", fwd.Messages[1]["content"])
}

func TestChat_PromptCompressed_NoOpUnderBudget(t *testing.T) {
	cap := &upstreamCapture{}
	srv := newChatHandlerSrv(t, cap.handler(t))

	tk, err := tokenizer.NewSelector()
	require.NoError(t, err)

	srv.deps.Compressor = prompt.NewSlidingWindow(prompt.SlidingWindowOptions{
		Tokenizer:     tk,
		DefaultWindow: 10000,
		TriggerRatio:  0.8,
	})
	srv2, err := New(srv.deps)
	require.NoError(t, err)

	body := chatBody("test-model", "hello", false)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("X-X-Beacon-Prompt-Compressed"),
		"under-budget request must not set the compressed header")
}
