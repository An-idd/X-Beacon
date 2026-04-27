package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLoadRegistry_MissingFile_ReturnsEmpty(t *testing.T) {
	reg, err := loadRegistry("/nonexistent/providers.yaml", zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, reg)
	assert.Empty(t, reg.Names())
	assert.Empty(t, reg.AllModels())
}

func TestLoadRegistry_MalformedFile_Fatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte("::: bad :::"), 0o600))

	_, err := loadRegistry(path, zap.NewNop())
	require.Error(t, err)
}

func TestLoadRegistry_Valid_ReturnsPopulated(t *testing.T) {
	yaml := `
providers:
  - name: openai
    type: openai
    api_key: sk-x
    models:
      exact: ["gpt-4o"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	reg, err := loadRegistry(path, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, []string{"openai"}, reg.Names())
}
