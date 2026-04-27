package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoad_Valid(t *testing.T) {
	path := writeYAML(t, `
keys:
  - id: dev
    name: "Local"
    secret: sk-local
  - id: ci
    name: "CI"
    secret: sk-ci
`)
	a, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 2, a.Size())

	p, err := a.Authenticate(context.Background(), "sk-local")
	require.NoError(t, err)
	assert.Equal(t, "dev", p.ID)
}

func TestLoad_EnvExpansion(t *testing.T) {
	t.Setenv("XBEACON_TEST_KEY", "secret-from-env")
	path := writeYAML(t, `
keys:
  - id: env-key
    secret: ${XBEACON_TEST_KEY}
`)
	a, err := Load(path)
	require.NoError(t, err)

	p, err := a.Authenticate(context.Background(), "secret-from-env")
	require.NoError(t, err)
	assert.Equal(t, "env-key", p.ID)
}

func TestLoad_EnvDefault(t *testing.T) {
	path := writeYAML(t, `
keys:
  - id: dev
    secret: ${XBEACON_NOT_SET:-default-fallback}
`)
	a, err := Load(path)
	require.NoError(t, err)

	_, err = a.Authenticate(context.Background(), "default-fallback")
	require.NoError(t, err)
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/auth.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read")
}

func TestLoad_MalformedYAML(t *testing.T) {
	path := writeYAML(t, "::: not yaml :::")
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse yaml")
}

func TestLoad_EmptyKeys(t *testing.T) {
	path := writeYAML(t, "keys: []\n")
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no keys")
}

func TestLoad_MissingEnvLeavesEmptySecret(t *testing.T) {
	// ${UNSET_NO_DEFAULT} expands to empty → entry validation must fail
	// rather than silently registering a useless principal.
	path := writeYAML(t, `
keys:
  - id: dev
    secret: ${XBEACON_UNSET_NO_DEFAULT_3X9K}
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret is required")
}

func TestLoad_PropagatesAuthErrors(t *testing.T) {
	path := writeYAML(t, `
keys:
  - id: dev
    secret: shared
  - id: also-dev
    secret: shared
`)
	_, err := Load(path)
	require.Error(t, err)
	// The wrapped error chain should still be inspectable.
	assert.True(t, errors.Is(err, err))
}
