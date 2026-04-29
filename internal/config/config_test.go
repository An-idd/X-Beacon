package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.Server.Addr)
	assert.Equal(t, 10*time.Second, cfg.Server.ReadTimeout)
	assert.Equal(t, "info", cfg.Log.Level)
	assert.True(t, cfg.Observability.Metrics.Enabled)
	assert.False(t, cfg.Observability.Tracing.Enabled)
	assert.InDelta(t, 0.95, cfg.Cache.Semantic.Threshold, 1e-9)
}

func TestLoad_YAMLOverrides(t *testing.T) {
	yaml := `
server:
  addr: ":9090"
  read_timeout: 5s
log:
  level: debug
`
	path := writeTempYAML(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.Server.Addr)
	assert.Equal(t, 5*time.Second, cfg.Server.ReadTimeout)
	assert.Equal(t, "debug", cfg.Log.Level)
	assert.Equal(t, 30*time.Second, cfg.Server.ShutdownTimeout) // default
}

func TestLoad_RoutingRulesParsed(t *testing.T) {
	yaml := `
routing:
  enabled: true
  rules:
    - name: translate
      route_to: gpt-4o-mini
      when:
        max_tokens: 200
        keywords_any: [translate, 翻译]
    - name: debug
      route_to: claude-3-5-sonnet
      when:
        keywords_none: [hello]
`
	path := writeTempYAML(t, yaml)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Routing.Enabled)
	require.Len(t, cfg.Routing.Rules, 2)

	r1 := cfg.Routing.Rules[0]
	assert.Equal(t, "translate", r1.Name)
	assert.Equal(t, "gpt-4o-mini", r1.RouteTo)
	assert.Equal(t, 200, r1.When.MaxTokens)
	assert.Equal(t, []string{"translate", "翻译"}, r1.When.KeywordsAny)

	r2 := cfg.Routing.Rules[1]
	assert.Equal(t, "claude-3-5-sonnet", r2.RouteTo)
	assert.Equal(t, []string{"hello"}, r2.When.KeywordsNone)
}

func TestLoad_RoutingDefaultsDisabled(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	assert.False(t, cfg.Routing.Enabled, "routing must default to off when absent from config")
	assert.Empty(t, cfg.Routing.Rules)
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("XBEACON_SERVER_ADDR", ":7777")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, ":7777", cfg.Server.Addr)
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	require.Error(t, err)
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTempYAML(t, "::: invalid yaml :::")
	_, err := Load(path)
	require.Error(t, err)
}

func TestValidate_TracingEnabledWithoutEndpoint(t *testing.T) {
	yaml := `
observability:
  tracing:
    enabled: true
`
	path := writeTempYAML(t, yaml)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracing.endpoint")
}

func TestValidate_InvalidLogLevel(t *testing.T) {
	yaml := `
log:
  level: loud
`
	path := writeTempYAML(t, yaml)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log.level")
}

func TestValidate_SemanticCacheOutOfRange(t *testing.T) {
	yaml := `
cache:
  semantic:
    enabled: true
    threshold: 1.5
    top_k: 0
`
	path := writeTempYAML(t, yaml)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "threshold")
	assert.Contains(t, err.Error(), "top_k")
}

func TestValidate_SampleRatioOutOfRange(t *testing.T) {
	yaml := `
observability:
  tracing:
    sample_ratio: 2.0
`
	path := writeTempYAML(t, yaml)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sample_ratio")
}

// TestLoad_ExampleConfig guards that the committed example stays valid.
func TestLoad_ExampleConfig(t *testing.T) {
	cfg, err := Load("../../configs/config.example.yaml")
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Server.Addr)
}
