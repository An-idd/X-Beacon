// Package catalog is X-Beacon's built-in knowledge base of well-known LLM
// models: their context window, capabilities, data policy, and default
// pricing. It exists to populate the X-Beacon extension fields on
// /v1/models responses without forcing operators to hand-author this data
// per deployment.
//
// Scope: this is *static* knowledge baked into the binary. Runtime prices
// configured via the admin API (model_pricing table) override the catalog
// defaults. Operators who want to add new models simply ship a PR with a
// new Entry; subsequent releases will let this layer be replaced by the
// preset library (configs/providers/presets/*.yaml).
//
// Concurrency: the map is built once in init() and read-only afterward;
// Lookup is safe for concurrent use.
package catalog

import "github.com/An-idd/x-beacon/internal/provider"

// Entry is one model's catalog metadata. All fields are optional from the
// caller's standpoint; a missing entry yields a (Entry{}, false) Lookup
// and the /v1/models handler will simply omit the extension fields for
// that model.
type Entry struct {
	// ContextLength is the maximum total tokens (prompt + completion) the
	// model accepts in one request.
	ContextLength int

	// Capabilities lists feature flags clients can rely on. See
	// internal/provider for the canonical constants.
	Capabilities []string

	// DataPolicy describes how the upstream treats request data.
	DataPolicy *provider.DataPolicy

	// DefaultPromptPer1kMicro / DefaultCompletionPer1kMicro are the
	// fallback prices used when the admin pricing table has no row for
	// this model. Zero means "no default published" — the handler will
	// omit the pricing field entirely rather than show $0.
	DefaultPromptPer1kMicro     int64
	DefaultCompletionPer1kMicro int64
	DefaultCurrency             string
}

// Lookup returns the catalog entry for a model ID. The boolean second
// return is false when the model is unknown to the catalog; callers
// should treat that as "no extension data available" and not an error.
func Lookup(modelID string) (Entry, bool) {
	e, ok := builtin[modelID]
	return e, ok
}

// All returns a snapshot of every catalog entry keyed by model ID. Used
// by tests to assert coverage; not intended for hot-path use.
func All() map[string]Entry {
	out := make(map[string]Entry, len(builtin))
	for k, v := range builtin {
		out[k] = v
	}
	return out
}

// builtin is the baseline catalog. Prices reflect upstream rate cards as
// of 2026-05-22 in USD per 1M tokens, converted to micro-USD per 1k
// tokens (multiply USD/1M by 1000 → USD/1k * 10^6 = micro/1k; equivalent
// to: USD per 1M tokens * 1000 = micro-USD per 1k tokens).
//
// To update a price: edit the constant and ship a release. Operators who
// need an immediate override should use the admin pricing API, which
// supersedes this table at runtime.
var builtin = map[string]Entry{
	// ----- OpenAI -----
	"gpt-4o": {
		ContextLength: 128_000,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityTools,
			provider.CapabilityVision, provider.CapabilityStream,
			provider.CapabilityJSONMode, provider.CapabilityLogprobs,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "opt_out", RetentionDays: 30},
		DefaultPromptPer1kMicro:     2_500,  // $2.50 / 1M tokens
		DefaultCompletionPer1kMicro: 10_000, // $10.00 / 1M tokens
		DefaultCurrency:             "USD",
	},
	"gpt-4o-mini": {
		ContextLength: 128_000,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityTools,
			provider.CapabilityVision, provider.CapabilityStream,
			provider.CapabilityJSONMode, provider.CapabilityLogprobs,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "opt_out", RetentionDays: 30},
		DefaultPromptPer1kMicro:     150, // $0.15 / 1M tokens
		DefaultCompletionPer1kMicro: 600, // $0.60 / 1M tokens
		DefaultCurrency:             "USD",
	},
	"gpt-4-turbo": {
		ContextLength: 128_000,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityTools,
			provider.CapabilityVision, provider.CapabilityStream,
			provider.CapabilityJSONMode,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "opt_out", RetentionDays: 30},
		DefaultPromptPer1kMicro:     10_000, // $10 / 1M
		DefaultCompletionPer1kMicro: 30_000, // $30 / 1M
		DefaultCurrency:             "USD",
	},
	"gpt-3.5-turbo": {
		ContextLength: 16_385,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityTools,
			provider.CapabilityStream, provider.CapabilityJSONMode,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "opt_out", RetentionDays: 30},
		DefaultPromptPer1kMicro:     500,   // $0.50 / 1M
		DefaultCompletionPer1kMicro: 1_500, // $1.50 / 1M
		DefaultCurrency:             "USD",
	},

	// ----- Anthropic -----
	"claude-3-5-sonnet-20241022": {
		ContextLength: 200_000,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityTools,
			provider.CapabilityVision, provider.CapabilityStream,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "opt_out", RetentionDays: 30},
		DefaultPromptPer1kMicro:     3_000,  // $3 / 1M
		DefaultCompletionPer1kMicro: 15_000, // $15 / 1M
		DefaultCurrency:             "USD",
	},
	"claude-3-5-haiku-20241022": {
		ContextLength: 200_000,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityTools,
			provider.CapabilityStream,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "opt_out", RetentionDays: 30},
		DefaultPromptPer1kMicro:     800,   // $0.80 / 1M
		DefaultCompletionPer1kMicro: 4_000, // $4 / 1M
		DefaultCurrency:             "USD",
	},
	"claude-3-opus-20240229": {
		ContextLength: 200_000,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityTools,
			provider.CapabilityVision, provider.CapabilityStream,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "opt_out", RetentionDays: 30},
		DefaultPromptPer1kMicro:     15_000, // $15 / 1M
		DefaultCompletionPer1kMicro: 75_000, // $75 / 1M
		DefaultCurrency:             "USD",
	},

	// ----- DeepSeek -----
	// Note: DeepSeek's published data policy is less explicit than the
	// major US providers; we mark it "unknown" rather than guess.
	"deepseek-chat": {
		ContextLength: 65_536,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityTools,
			provider.CapabilityStream, provider.CapabilityJSONMode,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "unknown"},
		DefaultPromptPer1kMicro:     140, // $0.14 / 1M
		DefaultCompletionPer1kMicro: 280, // $0.28 / 1M
		DefaultCurrency:             "USD",
	},
	"deepseek-reasoner": {
		ContextLength: 65_536,
		Capabilities: []string{
			provider.CapabilityChat, provider.CapabilityStream,
		},
		DataPolicy:                  &provider.DataPolicy{Training: "unknown"},
		DefaultPromptPer1kMicro:     550,   // $0.55 / 1M
		DefaultCompletionPer1kMicro: 2_190, // $2.19 / 1M
		DefaultCurrency:             "USD",
	},
}
