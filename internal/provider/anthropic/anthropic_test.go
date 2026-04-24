package anthropic

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := New(Config{
		Name:     "anthropic-test",
		Endpoint: srv.URL,
		APIKey:   "sk-ant-test",
		Timeout:  5 * time.Second,
		Models:   Models{Exact: []string{"claude-3-5-sonnet-20241022"}},
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
	p, err := New(Config{Name: "anthropic", APIKey: "sk-x"})
	require.NoError(t, err)
	assert.Equal(t, "https://api.anthropic.com", p.baseURL)
	assert.Equal(t, defaultTimeout, p.cfg.Timeout)
	assert.Equal(t, defaultAPIVersion, p.cfg.APIVersion)
	assert.Equal(t, defaultMaxTokens, p.defaultMaxTokens)
}

func TestNew_EndpointTrailingSlashTrimmed(t *testing.T) {
	p, err := New(Config{Name: "x", APIKey: "sk-x", Endpoint: "https://host.example/"})
	require.NoError(t, err)
	assert.Equal(t, "https://host.example", p.baseURL)
}

func TestNew_CustomAPIVersion(t *testing.T) {
	p, err := New(Config{Name: "x", APIKey: "sk-x", APIVersion: "2024-01-01"})
	require.NoError(t, err)
	assert.Equal(t, "2024-01-01", p.cfg.APIVersion)
}

func TestNew_CustomDefaultMaxTokens(t *testing.T) {
	p, err := New(Config{Name: "x", APIKey: "sk-x", DefaultMaxTokens: 8192})
	require.NoError(t, err)
	assert.Equal(t, 8192, p.defaultMaxTokens)
}

func TestName(t *testing.T) {
	p, _ := New(Config{Name: "anthropic-primary", APIKey: "sk-x"})
	assert.Equal(t, "anthropic-primary", p.Name())
}

func TestSupportedModels_OwnedByAnthropic(t *testing.T) {
	p, _ := New(Config{
		Name:   "anthropic",
		APIKey: "sk-x",
		Models: Models{
			Exact: []string{"claude-3-5-sonnet-20241022", "claude-3-haiku-20240307"},
			Glob:  []string{"claude-*"}, // glob must not leak into SupportedModels
		},
	})
	models := p.SupportedModels()
	require.Len(t, models, 2)
	assert.Equal(t, "claude-3-5-sonnet-20241022", models[0].ID)
	assert.Equal(t, "anthropic", models[0].OwnedBy)
	assert.Equal(t, "model", models[0].Object)
}

func TestSetHeaders(t *testing.T) {
	p, _ := New(Config{Name: "anthropic", APIKey: "sk-ant-abc"})
	req, _ := http.NewRequest(http.MethodPost, "http://x/", strings.NewReader("{}"))
	p.setHeaders(req)

	assert.Equal(t, "sk-ant-abc", req.Header.Get("x-api-key"))
	assert.Empty(t, req.Header.Get("Authorization"), "must NOT use Authorization: Bearer")
	assert.Equal(t, defaultAPIVersion, req.Header.Get("anthropic-version"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Equal(t, userAgent, req.Header.Get("User-Agent"))
}

// Ensure errors package-level aliasing hasn't accidentally shadowed sentinels.
func TestErrors_NoShadowing(t *testing.T) {
	_, err := New(Config{})
	require.Error(t, err)
	assert.False(t, errors.Is(err, provider.ErrAuth))
}
