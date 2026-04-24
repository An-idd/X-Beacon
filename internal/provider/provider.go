// Package provider defines the unified abstraction over LLM service providers
// (OpenAI, Anthropic, DeepSeek, ...). All request/response types use
// OpenAI-compatible field names so existing OpenAI SDK code can flow through
// the gateway unchanged; each provider is responsible for converting between
// its own wire format and these types.
package provider

import "context"

// Provider is the unified interface every upstream LLM service adapter must
// implement. Implementations live in sub-packages (internal/provider/openai,
// internal/provider/anthropic, ...). The interface intentionally stays small
// in Week 1; Embeddings and HealthCheck are added in later weeks.
type Provider interface {
	// Name returns the provider's unique identifier as configured in
	// providers.yaml (e.g. "openai-primary", not the type "openai").
	Name() string

	// ChatCompletion issues a non-streaming chat request. Errors returned
	// should be wrapped against one of the package-level sentinels
	// (ErrAuth, ErrRateLimited, ...) so callers can use errors.Is.
	ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

	// ChatCompletionStream issues a streaming chat request. The returned
	// channel is closed by the implementation after the final chunk or an
	// error event; callers must drain it (or cancel ctx) to avoid leaks.
	// The returned error is non-nil only when stream setup fails before
	// the first event.
	ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamEvent, error)

	// SupportedModels returns the static set of models this provider can
	// serve. Used by /v1/models and by the registry to build the routing
	// table at startup.
	SupportedModels() []ModelInfo
}

// ModelInfo describes a single model served by a provider. The field names
// mirror the OpenAI /v1/models response.
type ModelInfo struct {
	ID       string `json:"id"`
	Object   string `json:"object"` // always "model"
	Created  int64  `json:"created,omitempty"`
	OwnedBy  string `json:"owned_by,omitempty"`
	Provider string `json:"-"` // gateway-side metadata, not exposed
}
