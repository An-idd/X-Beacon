package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForwardMessages_PassesBodyAndHeadersVerbatim(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message"}`))
	}))
	defer upstream.Close()

	p, err := New(Config{Name: "anth", APIKey: "sk-ant-test", Endpoint: upstream.URL})
	require.NoError(t, err)

	// Body with a thinking block — must reach upstream byte-identical.
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":10,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := p.ForwardMessages(context.Background(), body, "prompt-caching-2024-07-31")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, string(body), string(gotBody), "body must be forwarded verbatim")
	assert.Equal(t, "sk-ant-test", gotHeaders.Get("x-api-key"))
	assert.NotEmpty(t, gotHeaders.Get("anthropic-version"))
	assert.Equal(t, "prompt-caching-2024-07-31", gotHeaders.Get("anthropic-beta"),
		"client anthropic-beta header must pass through")

	out, _ := io.ReadAll(resp.Body)
	assert.JSONEq(t, `{"id":"msg_1","type":"message"}`, string(out))
}

func TestForwardMessages_NoBetaHeaderWhenEmpty(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Values("anthropic-beta"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	p, err := New(Config{Name: "anth", APIKey: "k", Endpoint: upstream.URL})
	require.NoError(t, err)
	resp, err := p.ForwardMessages(context.Background(), []byte(`{}`), "")
	require.NoError(t, err)
	resp.Body.Close()
}
