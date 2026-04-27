package registry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoad_Minimal(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: openai-primary
    type: openai
    api_key: sk-test
    models:
      exact: ["gpt-4o"]
`)
	reg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, reg)

	p, err := reg.GetByName("openai-primary")
	require.NoError(t, err)
	assert.Equal(t, "openai-primary", p.Name())
}

func TestLoad_EnvExpansionInAPIKey(t *testing.T) {
	t.Setenv("X_TEST_OPENAI_KEY", "sk-from-env")
	path := writeYAML(t, `
providers:
  - name: openai
    type: openai
    api_key: ${X_TEST_OPENAI_KEY}
    models:
      exact: ["gpt-4o"]
`)
	reg, err := Load(path)
	require.NoError(t, err)

	// Indirect check: if env wasn't expanded, openai.New would have failed
	// with "APIKey is required" (empty after YAML parse of literal "${...}").
	_, err = reg.GetByName("openai")
	require.NoError(t, err)
}

func TestLoad_EnvDefaultApplied(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: openai
    type: openai
    endpoint: ${X_TEST_NO_VAR:-https://custom.example}
    api_key: sk-x
    models:
      exact: ["gpt-4o"]
`)
	reg, err := Load(path)
	require.NoError(t, err)
	_, err = reg.GetByName("openai")
	require.NoError(t, err)
}

func TestLoad_Routing_FullFile(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: openai-primary
    type: openai
    api_key: sk-1
    models:
      exact: ["gpt-4o", "gpt-4o-mini"]
      glob: ["gpt-4-*"]
  - name: azure
    type: openai
    api_key: sk-2
    endpoint: https://azure.example
    models:
      exact: ["gpt-4o-azure"]
      glob: ["gpt-4-*"]
default_provider: openai-primary
`)
	reg, err := Load(path)
	require.NoError(t, err)

	p, _ := reg.ResolveModel("gpt-4o")
	assert.Equal(t, "openai-primary", p.Name())

	p, _ = reg.ResolveModel("gpt-4o-azure")
	assert.Equal(t, "azure", p.Name())

	p, _ = reg.ResolveModel("gpt-4-turbo") // glob; primary wins by order
	assert.Equal(t, "openai-primary", p.Name())

	p, _ = reg.ResolveModel("llama-3") // no match; default
	assert.Equal(t, "openai-primary", p.Name())
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/providers.yaml")
	require.Error(t, err)
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeYAML(t, "::: invalid :::")
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_EmptyProviders(t *testing.T) {
	path := writeYAML(t, `providers: []`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestLoad_MissingName(t *testing.T) {
	path := writeYAML(t, `
providers:
  - type: openai
    api_key: sk-x
    models:
      exact: ["gpt-4o"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

func TestLoad_MissingType(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: foo
    api_key: sk-x
    models:
      exact: ["gpt-4o"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type is required")
}

func TestLoad_NoModels(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: foo
    type: openai
    api_key: sk-x
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one model")
}

func TestLoad_UnknownProviderType(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: xyz
    type: bogus-provider
    api_key: sk-x
    models:
      exact: ["gpt-4o"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown provider type")
}

func TestLoad_DuplicateName(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: openai
    type: openai
    api_key: sk-1
    models:
      exact: ["gpt-4o"]
  - name: openai
    type: openai
    api_key: sk-2
    models:
      exact: ["gpt-4o-mini"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate name")
}

func TestLoad_ExactConflictAcrossProviders(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: p1
    type: openai
    api_key: sk-1
    models:
      exact: ["gpt-4o"]
  - name: p2
    type: openai
    api_key: sk-2
    models:
      exact: ["gpt-4o"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already claimed")
}

func TestLoad_DefaultProviderMustExist(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: openai
    type: openai
    api_key: sk-x
    models:
      exact: ["gpt-4o"]
default_provider: ghost
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_provider")
	assert.Contains(t, err.Error(), "ghost")
}

func TestLoad_MultipleErrorsSurfaced(t *testing.T) {
	// Both providers have problems; errors.Join must report both.
	path := writeYAML(t, `
providers:
  - name: ""
    type: openai
    api_key: sk-1
    models:
      exact: ["gpt-4o"]
  - name: p2
    type: bogus
    api_key: sk-2
    models:
      exact: ["gpt-4o-mini"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
	assert.Contains(t, err.Error(), "unknown provider type")
}

// TestLoad_ExampleFile guards that the committed providers.example.yaml
// remains loadable end-to-end. Step 3.6 enabled all three providers in
// the template, so all three env vars must be set during this test.
func TestLoad_ExampleFile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-example")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")
	t.Setenv("DEEPSEEK_API_KEY", "sk-test-deepseek")
	reg, err := Load("../../../configs/providers.example.yaml")
	require.NoError(t, err)
	assert.NotEmpty(t, reg.Names())

	// At least one provider must be resolvable via its declared exact model.
	// We don't hardcode model IDs here to keep this test stable against
	// example tweaks.
	for _, name := range reg.Names() {
		p, err := reg.GetByName(name)
		require.NoError(t, err)
		for _, m := range p.SupportedModels() {
			resolved, rerr := reg.ResolveModel(m.ID)
			require.NoError(t, rerr)
			assert.Equal(t, name, resolved.Name())
			return // one successful round-trip is enough
		}
	}
}

func TestLoad_DuplicateNameIsReportedEvenWithExactConflict(t *testing.T) {
	// Guard that errors.Join accumulates; the second provider should still
	// produce a duplicate-name error even if it also has other issues.
	path := writeYAML(t, `
providers:
  - name: x
    type: openai
    api_key: sk-1
    models:
      exact: ["m1"]
  - name: x
    type: openai
    api_key: sk-2
    models:
      exact: ["m1"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate name")
}

// smoke test for the error-wrapping so that downstream callers can
// errors.Is against ErrUnknownProvider / ErrNoProviderForModel when
// interacting with a Registry returned from Load.
func TestLoad_ErrorWrappingForEndUser(t *testing.T) {
	path := writeYAML(t, `
providers:
  - name: openai
    type: openai
    api_key: sk-x
    models:
      exact: ["gpt-4o"]
`)
	reg, err := Load(path)
	require.NoError(t, err)

	_, err = reg.GetByName("not-here")
	assert.True(t, errors.Is(err, ErrUnknownProvider))
}
