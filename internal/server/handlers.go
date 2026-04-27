package server

import (
	"encoding/json"
	"net/http"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// modelsResponse mirrors the OpenAI /v1/models response envelope.
type modelsResponse struct {
	Object string               `json:"object"`
	Data   []provider.ModelInfo `json:"data"`
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
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// healthzHandler is a minimal liveness probe. Kept stupid on purpose: any
// dependency check belongs in /readyz (Week 4).
func healthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
}
