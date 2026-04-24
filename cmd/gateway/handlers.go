package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// modelsResponse mirrors the OpenAI /v1/models response envelope:
//
//	{"object": "list", "data": [ModelInfo, ...]}
type modelsResponse struct {
	Object string                 `json:"object"`
	Data   []provider.ModelInfo   `json:"data"`
}

// modelsHandler returns a handler for GET /v1/models. The response format
// is OpenAI-compatible so existing SDKs discover the gateway's catalog
// without adaptation.
func modelsHandler(reg *registry.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		resp := modelsResponse{
			Object: "list",
			Data:   reg.AllModels(),
		}
		// Ensure non-nil slice so JSON renders `[]` instead of `null`.
		if resp.Data == nil {
			resp.Data = []provider.ModelInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		// Encode writes directly to the response; header is set above.
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// loadRegistry reads providers.yaml and constructs a Registry, tolerating
// the file's absence at startup with a clear warning. Any other error
// (permissions, malformed YAML, validation) is fatal.
func loadRegistry(path string, logger *zap.Logger) (*registry.Registry, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("providers file not found; gateway starts with empty registry",
				zap.String("path", path),
				zap.String("hint", "copy configs/providers.example.yaml and set API keys"))
			return registry.NewEmpty(), nil
		}
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	reg, err := registry.Load(path)
	if err != nil {
		return nil, err
	}
	logger.Info("providers loaded",
		zap.Strings("names", reg.Names()),
		zap.Int("models", len(reg.AllModels())))
	return reg, nil
}
