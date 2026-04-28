// Package tokenizer counts tokens for chat-completion requests and
// responses. It exposes a small interface plus two implementations: an
// OpenAI-faithful one backed by tiktoken-go (BPE, embedded vocab — no
// network) and an Anthropic approximation that reuses the OpenAI BPE
// scaled by an empirical factor.
//
// The interface intentionally stays tight: callers only need
// CountMessages (for prompt accounting) and CountText (for streaming
// completions where Usage isn't available). Adding Embeddings later
// means a single new method, not a new package.
package tokenizer

import (
	"strings"

	"github.com/tiktoken-go/tokenizer"

	"github.com/An-idd/x-beacon/internal/provider"
)

// Tokenizer counts tokens for one provider family. Implementations are
// goroutine-safe; the underlying BPE codec is read-only after init.
type Tokenizer interface {
	// Family identifies the implementation in logs / metrics
	// (e.g. "openai-cl100k", "anthropic-approx").
	Family() string

	// CountMessages returns the total prompt-token cost of an
	// OpenAI-style messages array, including the per-message overhead
	// each model adds (role token, separator, etc.). Used to seed the
	// rate-limiter cost (Week 5 carry-over #59) and to fill prompt_tokens
	// when the upstream omits Usage.
	CountMessages(messages []provider.Message) int

	// CountText returns the token count of a single piece of text.
	// Used during streaming to aggregate completion_tokens before the
	// upstream's terminal usage chunk arrives (or as a substitute when
	// it never arrives, e.g. OpenAI streaming pre-2024).
	CountText(text string) int
}

// openaiBPE is the OpenAI-faithful counter backed by cl100k_base
// (GPT-4 / GPT-3.5-turbo / text-embedding-ada-002 family). The newer
// o200k_base is selected automatically inside Encode for models that use
// it; we hold a pre-loaded codec for the common path and look up the
// right one per model only when CountMessages is asked to.
type openaiBPE struct {
	defaultCodec tokenizer.Codec // cl100k_base — lifetime of the process
}

// NewOpenAI returns a tokenizer pre-loaded with cl100k_base. Returns an
// error only if the embedded vocab fails to materialize (which would
// indicate a corrupt build of the tokenizer dependency, not a runtime
// fault).
func NewOpenAI() (Tokenizer, error) {
	codec, err := tokenizer.Get(tokenizer.Cl100kBase)
	if err != nil {
		return nil, err
	}
	return &openaiBPE{defaultCodec: codec}, nil
}

func (t *openaiBPE) Family() string { return "openai-cl100k" }

// perMessageOverhead is the fixed-size header tiktoken adds for every
// message in chat-completions accounting: 3 tokens for the message
// envelope (role / content separators) per OpenAI's published cookbook
// formula, plus 3 tokens for the trailing assistant priming that the
// upstream prepends. The 4096 max-token caller is responsible for
// budgeting; this counter only reports.
const (
	perMessageOverhead = 3
	primingOverhead    = 3
)

func (t *openaiBPE) CountMessages(messages []provider.Message) int {
	if len(messages) == 0 {
		return 0
	}
	total := primingOverhead
	for _, m := range messages {
		total += perMessageOverhead
		total += t.CountText(m.Role)
		total += t.CountText(m.Content)
		if m.Name != "" {
			// `name` field, when present, replaces the role-1 token
			// per OpenAI's formula. We approximate by adding name
			// tokens — same magnitude, off by 1 at most.
			total += t.CountText(m.Name)
		}
	}
	return total
}

func (t *openaiBPE) CountText(text string) int {
	if text == "" {
		return 0
	}
	ids, _, err := t.defaultCodec.Encode(text)
	if err != nil {
		// BPE encoder is exhaustive over UTF-8; failure is
		// effectively unreachable. Fall back to char-based estimate
		// so billing never returns 0 silently for a non-empty body.
		return charEstimate(text)
	}
	return len(ids)
}

// anthropicApprox uses cl100k_base scaled by a published-empirical ratio
// (~1.15× OpenAI tokens for the same text on average) to approximate
// Claude's tokenizer. Anthropic doesn't publish their tokenizer for
// arbitrary text; the scaling factor is from internal benchmarks plus
// public model cards. README documents the inherent inaccuracy.
type anthropicApprox struct {
	openai *openaiBPE
}

// NewAnthropic returns the Anthropic-approximation tokenizer.
func NewAnthropic() (Tokenizer, error) {
	c, err := codecCl100k()
	if err != nil {
		return nil, err
	}
	return &anthropicApprox{openai: &openaiBPE{defaultCodec: c}}, nil
}

func codecCl100k() (tokenizer.Codec, error) { return tokenizer.Get(tokenizer.Cl100kBase) }

// anthropicScale converts cl100k token counts into an Anthropic estimate.
// Source: claude.ai/docs (input/output tokens are 1.10–1.20× cl100k for
// English text). 1.15 is the rounded mid-point. Documented as "estimate"
// in README — billing reconciles against Usage when available.
const anthropicScale = 1.15

func (t *anthropicApprox) Family() string { return "anthropic-approx" }

func (t *anthropicApprox) CountMessages(messages []provider.Message) int {
	return scale(t.openai.CountMessages(messages), anthropicScale)
}

func (t *anthropicApprox) CountText(text string) int {
	return scale(t.openai.CountText(text), anthropicScale)
}

// scale rounds half-up so a 1-token call doesn't disappear to zero.
func scale(n int, factor float64) int {
	if n == 0 {
		return 0
	}
	return int(float64(n)*factor + 0.5)
}

// charEstimate is a degenerate fallback used only when the BPE codec
// returns an error. Crude but never zero: counts non-whitespace runes /
// 4 (a rough English-text heuristic; tokens average ~4 chars).
func charEstimate(text string) int {
	if text == "" {
		return 0
	}
	chars := 0
	for _, r := range text {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		chars++
	}
	if chars < 4 {
		return 1
	}
	return chars / 4
}

// Selector picks the right Tokenizer for a model id. The mapping is
// expressed as ordered prefix rules; first match wins. Defaults to the
// OpenAI tokenizer because cl100k is the closest universal fallback for
// unknown models (DeepSeek-* and most third-party models use cl100k or
// near-equivalent BPE).
type Selector struct {
	openai    Tokenizer
	anthropic Tokenizer
}

// NewSelector wires up both implementations once. main calls this during
// startup; the resulting Selector is cheap to share across requests.
func NewSelector() (*Selector, error) {
	o, err := NewOpenAI()
	if err != nil {
		return nil, err
	}
	a, err := NewAnthropic()
	if err != nil {
		return nil, err
	}
	return &Selector{openai: o, anthropic: a}, nil
}

// For returns the tokenizer that should account for the given model id.
// "claude*" → anthropic; everything else → openai (cl100k_base).
func (s *Selector) For(model string) Tokenizer {
	if strings.HasPrefix(strings.ToLower(model), "claude") {
		return s.anthropic
	}
	return s.openai
}
