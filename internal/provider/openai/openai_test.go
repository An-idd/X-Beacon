package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/pkg/version"
)

// newTestProvider spins up an httptest server running handler and returns a
// Provider pointing at it. The server is cleaned up on test end.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := New(Config{
		Name:         "openai-test",
		Endpoint:     srv.URL,
		APIKey:       "sk-test",
		Organization: "org-test",
		Timeout:      5 * time.Second,
		Models:       Models{Exact: []string{"gpt-4o-mini", "gpt-4o"}},
	})
	require.NoError(t, err)
	return p
}

func TestNew_RequiresName(t *testing.T) {
	_, err := New(Config{APIKey: "sk-x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Name")
}

func TestNew_RequiresAPIKey(t *testing.T) {
	_, err := New(Config{Name: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APIKey")
}

func TestNew_DefaultsApplied(t *testing.T) {
	p, err := New(Config{Name: "openai", APIKey: "sk-x"})
	require.NoError(t, err)
	assert.Equal(t, "https://api.openai.com", p.baseURL)
	assert.Equal(t, defaultTimeout, p.cfg.Timeout)
}

func TestNew_EndpointTrailingSlashTrimmed(t *testing.T) {
	p, err := New(Config{Name: "x", APIKey: "sk-x", Endpoint: "https://host.example/"})
	require.NoError(t, err)
	assert.Equal(t, "https://host.example", p.baseURL)
}

func TestName(t *testing.T) {
	p, _ := New(Config{Name: "openai-primary", APIKey: "sk-x"})
	assert.Equal(t, "openai-primary", p.Name())
}

func TestSupportedModels(t *testing.T) {
	p, _ := New(Config{
		Name:   "openai-primary",
		APIKey: "sk-x",
		Models: Models{
			Exact: []string{"gpt-4o", "gpt-4o-mini"},
			Glob:  []string{"gpt-4-*"}, // must NOT appear
		},
	})
	models := p.SupportedModels()
	require.Len(t, models, 2)
	assert.Equal(t, "gpt-4o", models[0].ID)
	assert.Equal(t, "model", models[0].Object)
	assert.Equal(t, "openai-primary", models[0].Provider)
	assert.Equal(t, "openai", models[0].OwnedBy)
}

func TestSetHeaders(t *testing.T) {
	p, _ := New(Config{
		Name:         "openai",
		APIKey:       "sk-secret",
		Organization: "org-abc",
	})
	req, _ := http.NewRequest(http.MethodPost, "http://x/", strings.NewReader("{}"))
	p.setHeaders(req)

	assert.Equal(t, "Bearer sk-secret", req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Equal(t, "application/json", req.Header.Get("Accept"))
	assert.Equal(t, version.UserAgent(), req.Header.Get("User-Agent"))
	assert.Equal(t, "org-abc", req.Header.Get("OpenAI-Organization"))
}

func TestSetHeaders_NoOrgWhenEmpty(t *testing.T) {
	p, _ := New(Config{Name: "x", APIKey: "sk-x"})
	req, _ := http.NewRequest(http.MethodPost, "http://x/", strings.NewReader("{}"))
	p.setHeaders(req)
	assert.Empty(t, req.Header.Get("OpenAI-Organization"))
}
