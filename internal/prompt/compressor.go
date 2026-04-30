// Package prompt holds the Week 12 context-truncation layer. The
// motivation is correctness more than cost: long contexts degrade
// quality (OpenAI / Anthropic both document this) and risk hitting
// the upstream's hard context-window limit, which is a 4xx the client
// can do nothing about.
//
// The MVP ships exactly one strategy — system-preserving sliding
// window with a token budget cap — picked because it is the
// simplest defensible default. Anything fancier (importance scoring,
// summarization, embedding-based selection) would need data to
// justify; per CLAUDE.md L156 we don't build it before we need it.
//
// Compression mutates req.Messages in place to stay consistent with
// the smart-router (Week 11), which also rewrites req in place. The
// chat handler runs Compress *after* Classify so the routing
// decision sees the original prompt; the cache key is computed
// *after* Compress so the truncated form is what's cached. This
// pairing means clients sending "the same long prompt" hit the same
// cache slot regardless of which side of the trigger ratio they're
// on.
package prompt

import (
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
)

// Compressor trims chat history to fit a model's context window.
// Implementations must be safe for concurrent use; Compress is pure
// (no IO) because it runs on every request and mustn't block.
type Compressor interface {
	// Compress mutates req.Messages in place if trimming was needed.
	// Returns a Result describing what happened. A no-op call (under
	// the trigger threshold) returns Result{Compressed: false} with
	// TokensBefore == TokensAfter.
	Compress(req *provider.ChatRequest) Result
}

// Result describes the outcome of one Compress call. Surfaced both
// to the chat handler (for response headers / logging) and to
// metrics (counters + histograms).
type Result struct {
	// Compressed is true when at least one message was dropped.
	Compressed bool

	// TokensBefore is the prompt-token count of req.Messages prior
	// to trimming. Always populated when a tokenizer is wired.
	TokensBefore int

	// TokensAfter is the prompt-token count after trimming. Equal
	// to TokensBefore on no-op.
	TokensAfter int

	// RemovedMessages counts how many non-system messages were
	// dropped. System messages are always preserved, so this is
	// also the lower bound on "lost turns".
	RemovedMessages int
}

// SlidingWindow keeps all system messages plus the most-recent
// non-system messages that fit within a token budget. The budget
// is `window * triggerRatio`, where window is the model-specific
// context size; `triggerRatio` doubles as "compress when above"
// and "compress to fit under" — one knob covers both.
//
// A floor (MinKeepMessages) overrides the budget: if removing
// further would drop the tail below the floor, compression stops
// even if the prompt still exceeds budget. The upstream then
// 4xx's on context_length_exceeded, which is the correct signal
// to a client that's pasting an oversized single message — we
// can't safely truncate that for them.
type SlidingWindow struct {
	tk             *tokenizer.Selector
	defaultWindow  int
	windows        map[string]int
	triggerRatio   float64
	minKeepMessages int
}

// SlidingWindowOptions configures the sliding-window compressor.
// All fields have sane fallbacks so a zero-value SlidingWindow is
// not allowed but partial configs are.
type SlidingWindowOptions struct {
	// Tokenizer is required. nil → NewSlidingWindow returns nil
	// (compression disabled).
	Tokenizer *tokenizer.Selector

	// DefaultWindow is the context-window size in tokens when the
	// model id isn't in Windows. 0 → 128_000 (covers gpt-4o /
	// claude-3.5; smaller models truncate sooner via Windows map).
	DefaultWindow int

	// Windows maps model id → context size in tokens. Empty map
	// means every model uses DefaultWindow.
	Windows map[string]int

	// TriggerRatio in (0, 1]. Compression engages when prompt
	// tokens > Window * TriggerRatio, and trims back under the
	// same line. 0 → 0.8 (leave headroom for the response).
	TriggerRatio float64

	// MinKeepMessages is the floor on non-system messages kept,
	// even when the budget says drop more. 0 → 2 (one user/
	// assistant turn). System messages are not counted here.
	MinKeepMessages int
}

const (
	defaultWindowFallback   = 128_000
	defaultTriggerRatio     = 0.8
	defaultMinKeepMessages  = 2
)

// NewSlidingWindow returns a configured compressor or nil when
// Tokenizer is missing (which the caller should treat as "feature
// disabled"). Other fields fall back to defaults.
func NewSlidingWindow(opts SlidingWindowOptions) *SlidingWindow {
	if opts.Tokenizer == nil {
		return nil
	}
	c := &SlidingWindow{
		tk:              opts.Tokenizer,
		defaultWindow:   opts.DefaultWindow,
		windows:         opts.Windows,
		triggerRatio:    opts.TriggerRatio,
		minKeepMessages: opts.MinKeepMessages,
	}
	if c.defaultWindow <= 0 {
		c.defaultWindow = defaultWindowFallback
	}
	if c.triggerRatio <= 0 || c.triggerRatio > 1 {
		c.triggerRatio = defaultTriggerRatio
	}
	if c.minKeepMessages <= 0 {
		c.minKeepMessages = defaultMinKeepMessages
	}
	return c
}

// Compress walks req.Messages, partitions into (system, rest),
// then keeps a tail of `rest` whose tokens + system tokens fit
// the budget, with MinKeepMessages as a floor. System order is
// preserved at the front. Order within the kept tail is also
// preserved (drop-from-head, not from-middle).
func (c *SlidingWindow) Compress(req *provider.ChatRequest) Result {
	if c == nil || req == nil || len(req.Messages) == 0 {
		return Result{}
	}

	t := c.tk.For(req.Model)
	tokensBefore := t.CountMessages(req.Messages)
	window := c.windowFor(req.Model)
	budget := int(float64(window) * c.triggerRatio)

	if tokensBefore <= budget {
		return Result{TokensBefore: tokensBefore, TokensAfter: tokensBefore}
	}

	// Partition. System messages preserve their relative order
	// (multi-system prompts are rare but legal under OpenAI's API).
	systems := make([]provider.Message, 0, len(req.Messages))
	rest := make([]provider.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" {
			systems = append(systems, m)
		} else {
			rest = append(rest, m)
		}
	}

	systemTokens := 0
	if len(systems) > 0 {
		systemTokens = t.CountMessages(systems)
	}

	// Walk rest from the tail, accumulating until adding one more
	// would breach the budget. The floor (minKeepMessages) takes
	// precedence over the budget — see SlidingWindow doc.
	keepStart := len(rest) // index in `rest` where the kept tail begins
	keepTokens := 0
	for i := len(rest) - 1; i >= 0; i-- {
		msgTokens := t.CountMessages([]provider.Message{rest[i]})
		fits := systemTokens+keepTokens+msgTokens <= budget
		mustKeep := (len(rest) - i) <= c.minKeepMessages
		if !fits && !mustKeep {
			break
		}
		keepStart = i
		keepTokens += msgTokens
	}

	removed := keepStart
	if removed == 0 {
		// Nothing trimmable. The prompt is over budget but the
		// budget is dominated by system + the floor; we let the
		// upstream surface context_length_exceeded.
		return Result{TokensBefore: tokensBefore, TokensAfter: tokensBefore}
	}

	trimmed := make([]provider.Message, 0, len(systems)+(len(rest)-keepStart))
	trimmed = append(trimmed, systems...)
	trimmed = append(trimmed, rest[keepStart:]...)
	req.Messages = trimmed

	tokensAfter := t.CountMessages(req.Messages)
	return Result{
		Compressed:      true,
		TokensBefore:    tokensBefore,
		TokensAfter:     tokensAfter,
		RemovedMessages: removed,
	}
}

func (c *SlidingWindow) windowFor(model string) int {
	if w, ok := c.windows[model]; ok && w > 0 {
		return w
	}
	return c.defaultWindow
}

// Compile-time interface guard.
var _ Compressor = (*SlidingWindow)(nil)
