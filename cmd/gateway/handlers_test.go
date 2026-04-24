package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

func TestModelsHandler_EmptyRegistry_ReturnsListEnvelope(t *testing.T) {
	reg := registry.NewEmpty()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	modelsHandler(reg)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	// Decode as generic map so we catch accidental `null` vs `[]` mistakes.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	assert.Equal(t, "list", raw["object"])
	data, ok := raw["data"].([]any)
	require.True(t, ok, "data must be a JSON array, not null")
	assert.Empty(t, data)
}

func TestModelsHandler_WithRegistry_ReturnsSortedDedupedModels(t *testing.T) {
	yaml := `
providers:
  - name: openai
    type: openai
    api_key: sk-x
    models:
      exact: ["gpt-4o", "gpt-4o-mini"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	reg, err := registry.Load(path)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	modelsHandler(reg)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Object string                 `json:"object"`
		Data   []provider.ModelInfo   `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "list", resp.Object)
	require.Len(t, resp.Data, 2)

	// AllModels sorts by ID.
	assert.Equal(t, "gpt-4o", resp.Data[0].ID)
	assert.Equal(t, "gpt-4o-mini", resp.Data[1].ID)
	assert.Equal(t, "model", resp.Data[0].Object)

	// Internal Provider field must not leak into the response JSON.
	var rawBody map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rawBody))
	dataArr, _ := rawBody["data"].([]any)
	require.NotEmpty(t, dataArr)
	firstModel, _ := dataArr[0].(map[string]any)
	_, leaked := firstModel["Provider"]
	assert.False(t, leaked, "internal Provider field leaked to /v1/models response")
}

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
