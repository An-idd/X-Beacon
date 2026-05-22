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

// ModelInfo describes a single model served by a provider. The base four
// fields (ID/Object/Created/OwnedBy) mirror the OpenAI /v1/models response
// so existing SDKs parse the envelope unchanged. The remaining fields are
// X-Beacon extensions populated by the /v1/models handler from the catalog,
// pricing cache, and breaker gauges; absence is graceful via omitempty.
//
// Extension fields are placed at the top level (not under x_beacon:) to match
// the convention used by OpenRouter, Together, and Groq.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // always "model"
	Created int64  `json:"created,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`

	// X-Beacon extensions. All omitempty so models without catalog entries
	// produce an OpenAI-shape response.
	Pricing       *ModelPricing `json:"pricing,omitempty"`
	ContextLength int           `json:"context_length,omitempty"`
	Capabilities  []string      `json:"capabilities,omitempty"`
	DataPolicy    *DataPolicy   `json:"data_policy,omitempty"`
	Status        string        `json:"status,omitempty"` // admin-scope only; "available"|"degraded"|"unavailable"

	Provider string `json:"-"` // gateway-side metadata, not exposed
}

// ModelPricing is the per-1k-token unit price in human-readable string form.
// We serialize as strings (not floats) because float JSON round-tripping can
// turn "0.00014" into "0.00013999..." in some clients — for billing-adjacent
// metadata this is unacceptable. Internally pricing lives as int64 micro-USD
// (see internal/billing.Rate) and is converted at serialization time.
type ModelPricing struct {
	Prompt     string `json:"prompt"`     // e.g. "0.0025" — USD per 1K tokens
	Completion string `json:"completion"` // e.g. "0.01"
	Currency   string `json:"currency"`   // e.g. "USD"
	Unit       string `json:"unit"`       // e.g. "1K_tokens"
}

// DataPolicy describes how the upstream provider treats request data.
// Training: "opt_out" | "unknown" | "allowed". RetentionDays may be 0 when
// the provider doesn't publish a fixed retention policy.
type DataPolicy struct {
	Training      string `json:"training"`
	RetentionDays int    `json:"retention_days,omitempty"`
}

// Capability values used in ModelInfo.Capabilities. Free-form strings rather
// than an enum so adding a new capability ("audio", "code_interpreter", ...)
// is a one-line catalog edit, not a schema change.
const (
	CapabilityChat     = "chat"
	CapabilityTools    = "tools"
	CapabilityVision   = "vision"
	CapabilityStream   = "stream"
	CapabilityJSONMode = "json_mode"
	CapabilityLogprobs = "logprobs"
)
