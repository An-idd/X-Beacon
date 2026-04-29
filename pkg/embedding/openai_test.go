package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

// makeVector returns a slice of n float32 values; used to build mock
// upstream responses with the right dimension.
func makeVector(n int, fill float32) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = fill
	}
	return v
}

func newOpenAITestClient(t *testing.T, handler http.HandlerFunc, opts ...func(*OpenAIConfig)) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := OpenAIConfig{
		APIKey:     "sk-test",
		Endpoint:   srv.URL,
		Model:      "text-embedding-3-small",
		Dimensions: 4, // small for tests
		Timeout:    2 * time.Second,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := NewOpenAI(cfg)
	require.NoError(t, err)
	return c
}

func TestNewOpenAI_RequiresAPIKey(t *testing.T) {
	_, err := NewOpenAI(OpenAIConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APIKey")
}

func TestNewOpenAI_DefaultsApplied(t *testing.T) {
	c, err := NewOpenAI(OpenAIConfig{APIKey: "sk-test"})
	require.NoError(t, err)
	assert.Equal(t, "text-embedding-3-small", c.Model())
	assert.Equal(t, 1536, c.Dimensions())
}

func TestEmbed_HappyPath(t *testing.T) {
	var seen embedRequest
	handler := func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &seen))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": makeVector(4, 0.1)},
				{"index": 1, "embedding": makeVector(4, 0.2)},
			},
			"model": "text-embedding-3-small",
		})
	}
	c := newOpenAITestClient(t, handler)

	vecs, err := c.Embed(context.Background(), []string{"alpha", "beta"})
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	assert.Equal(t, makeVector(4, 0.1), vecs[0])
	assert.Equal(t, makeVector(4, 0.2), vecs[1])

	// Verify request shape: input + model + dimensions all forwarded.
	assert.Equal(t, []string{"alpha", "beta"}, seen.Input)
	assert.Equal(t, "text-embedding-3-small", seen.Model)
	assert.Equal(t, 4, seen.Dimensions)
}

func TestEmbed_DefendsAgainstUnorderedResponse(t *testing.T) {
	// Simulate an upstream that returns data[] out of order (spec
	// says ordered but we don't trust it). Sort by index must restore
	// alignment with the input slice.
	handler := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 1, "embedding": makeVector(4, 0.2)},
				{"index": 0, "embedding": makeVector(4, 0.1)},
			},
			"model": "text-embedding-3-small",
		})
	}
	c := newOpenAITestClient(t, handler)

	vecs, err := c.Embed(context.Background(), []string{"alpha", "beta"})
	require.NoError(t, err)
	assert.Equal(t, makeVector(4, 0.1), vecs[0], "vec[0] must match input[0] regardless of response order")
	assert.Equal(t, makeVector(4, 0.2), vecs[1])
}

func TestEmbed_EmptyInput(t *testing.T) {
	c := newOpenAITestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream must not be called on empty input")
	})
	_, err := c.Embed(context.Background(), nil)
	assert.True(t, errors.Is(err, ErrEmptyInput))

	_, err = c.Embed(context.Background(), []string{})
	assert.True(t, errors.Is(err, ErrEmptyInput))
}

func TestEmbed_DimensionMismatch(t *testing.T) {
	// Upstream returns a 8-dim vector but client expects 4. Should
	// error rather than silently truncate.
	handler := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": makeVector(8, 0.5)},
			},
		})
	}
	c := newOpenAITestClient(t, handler)
	_, err := c.Embed(context.Background(), []string{"x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dimension mismatch")
}

func TestEmbed_LengthMismatch(t *testing.T) {
	// Upstream returns fewer vectors than inputs.
	handler := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": makeVector(4, 0.1)},
			},
		})
	}
	c := newOpenAITestClient(t, handler)
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "length mismatch")
}

func TestEmbed_HTTPErrors(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantSentinel error
	}{
		{"401 auth", 401, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, provider.ErrAuth},
		{"403 auth", 403, `{"error":{"message":"forbidden"}}`, provider.ErrAuth},
		{"429 rate limited", 429, `{"error":{"message":"slow down"}}`, provider.ErrRateLimited},
		{"400 invalid", 400, `{"error":{"message":"bad input"}}`, provider.ErrInvalidRequest},
		{"422 invalid", 422, `{"error":{"message":"unprocessable"}}`, provider.ErrInvalidRequest},
		{"500 upstream", 500, `{"error":{"message":"oops"}}`, provider.ErrUpstream},
		{"503 upstream", 503, `{"error":{"message":"down"}}`, provider.ErrUpstream},
		{"malformed body falls back to upstream", 502, `<html>oops</html>`, provider.ErrUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}
			c := newOpenAITestClient(t, handler)
			_, err := c.Embed(context.Background(), []string{"x"})
			require.Error(t, err)
			var ue *provider.UpstreamError
			require.True(t, errors.As(err, &ue), "expected UpstreamError, got %T", err)
			assert.Equal(t, tc.status, ue.StatusCode)
			assert.True(t, errors.Is(err, tc.wantSentinel),
				"want sentinel %v, got %v", tc.wantSentinel, err)
		})
	}
}

func TestEmbed_ContextCanceled(t *testing.T) {
	c := newOpenAITestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream must not be reached on pre-cancelled ctx")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Embed(ctx, []string{"x"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestEmbed_Timeout(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		// Block past our 50 ms test timeout.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}
	c := newOpenAITestClient(t, handler, func(c *OpenAIConfig) {
		c.Timeout = 50 * time.Millisecond
	})
	_, err := c.Embed(context.Background(), []string{"x"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrTimeout))
}

func TestEmbed_DimensionsParameterOmittedWhenZero(t *testing.T) {
	// When user constructs OpenAIConfig.Dimensions=0 we default to
	// 1536 internally but should NOT forward `dimensions` in the
	// payload (server uses model native).
	var seen embedRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &seen))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": makeVector(1536, 0.01)},
			},
		})
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := NewOpenAI(OpenAIConfig{
		APIKey:   "sk-test",
		Endpoint: srv.URL,
		Timeout:  2 * time.Second,
	})
	require.NoError(t, err)
	_, err = c.Embed(context.Background(), []string{"x"})
	require.NoError(t, err)
	assert.Equal(t, 0, seen.Dimensions, "Dimensions=0 must not appear in request body")
}
