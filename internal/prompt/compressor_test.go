package prompt

import (
	"strings"
	"testing"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
)

func newTestSelector(t *testing.T) *tokenizer.Selector {
	t.Helper()
	sel, err := tokenizer.NewSelector()
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	return sel
}

// Pad returns a string that tokenizes to roughly n words (and thus
// at least n tokens). Used to engineer prompts above/below budget.
func pad(n int) string {
	return strings.Repeat("word ", n)
}

func TestNewSlidingWindow_NilTokenizerReturnsNil(t *testing.T) {
	if c := NewSlidingWindow(SlidingWindowOptions{}); c != nil {
		t.Fatalf("expected nil compressor when tokenizer is missing, got %#v", c)
	}
}

func TestSlidingWindow_NoOpWhenUnderBudget(t *testing.T) {
	c := NewSlidingWindow(SlidingWindowOptions{
		Tokenizer:     newTestSelector(t),
		DefaultWindow: 1000,
		TriggerRatio:  0.8,
	})
	req := &provider.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []provider.Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "hello"},
		},
	}
	res := c.Compress(req)
	if res.Compressed {
		t.Fatalf("expected no compression under budget, got %+v", res)
	}
	if got := len(req.Messages); got != 2 {
		t.Fatalf("messages mutated unexpectedly: got %d", got)
	}
	if res.TokensBefore != res.TokensAfter || res.TokensBefore == 0 {
		t.Fatalf("token bookkeeping wrong: %+v", res)
	}
}

func TestSlidingWindow_TrimsOldNonSystemMessages(t *testing.T) {
	// Tiny window forces compression on a multi-turn conversation.
	c := NewSlidingWindow(SlidingWindowOptions{
		Tokenizer:       newTestSelector(t),
		DefaultWindow:   100,
		TriggerRatio:    0.5, // budget = 50 tokens
		MinKeepMessages: 1,
	})
	req := &provider.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []provider.Message{
			{Role: "system", Content: "you are concise"},
			{Role: "user", Content: pad(80)}, // far over budget
			{Role: "assistant", Content: pad(80)},
			{Role: "user", Content: pad(80)},
			{Role: "assistant", Content: pad(80)},
			{Role: "user", Content: "what now"}, // small tail
		},
	}
	res := c.Compress(req)
	if !res.Compressed {
		t.Fatalf("expected compression: %+v\n  msgs left=%d", res, len(req.Messages))
	}
	if res.RemovedMessages == 0 {
		t.Fatal("RemovedMessages should be > 0")
	}
	// system should still be there at index 0.
	if req.Messages[0].Role != "system" {
		t.Fatalf("system message dropped or reordered: roles=%v", roles(req.Messages))
	}
	// Tail must include the most recent user message.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || last.Content != "what now" {
		t.Fatalf("tail not preserved: %+v", last)
	}
}

func TestSlidingWindow_PreservesAllSystemMessages(t *testing.T) {
	c := NewSlidingWindow(SlidingWindowOptions{
		Tokenizer:       newTestSelector(t),
		DefaultWindow:   200,
		TriggerRatio:    0.5,
		MinKeepMessages: 1,
	})
	req := &provider.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []provider.Message{
			{Role: "system", Content: "rule one"},
			{Role: "system", Content: "rule two"},
			{Role: "user", Content: pad(120)},
			{Role: "assistant", Content: pad(120)},
			{Role: "user", Content: "tail"},
		},
	}
	res := c.Compress(req)
	if !res.Compressed {
		t.Fatalf("expected compression: %+v", res)
	}
	gotSystems := 0
	for _, m := range req.Messages {
		if m.Role == "system" {
			gotSystems++
		}
	}
	if gotSystems != 2 {
		t.Fatalf("expected 2 system messages preserved, got %d (roles=%v)", gotSystems, roles(req.Messages))
	}
}

func TestSlidingWindow_FloorOverridesBudget(t *testing.T) {
	// Single huge user message; min_keep=1 means we cannot drop it
	// even though it dwarfs the budget. Compress should be a no-op
	// in this case so the upstream surfaces the real error.
	c := NewSlidingWindow(SlidingWindowOptions{
		Tokenizer:       newTestSelector(t),
		DefaultWindow:   100,
		TriggerRatio:    0.5,
		MinKeepMessages: 1,
	})
	req := &provider.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []provider.Message{
			{Role: "user", Content: pad(500)},
		},
	}
	res := c.Compress(req)
	if res.Compressed {
		t.Fatalf("expected no-op when only one message exists, got %+v", res)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages mutated: got %d", len(req.Messages))
	}
}

func TestSlidingWindow_PerModelWindowOverride(t *testing.T) {
	// Same prompt, two models — only the cheaper one with a small
	// window should trigger compression.
	c := NewSlidingWindow(SlidingWindowOptions{
		Tokenizer:     newTestSelector(t),
		DefaultWindow: 10000,
		Windows:       map[string]int{"tiny-model": 100},
		TriggerRatio:  0.5,
	})

	bigWindow := &provider.ChatRequest{
		Model: "gpt-4o",
		Messages: []provider.Message{
			{Role: "user", Content: pad(80)},
			{Role: "assistant", Content: pad(80)},
			{Role: "user", Content: "tail"},
		},
	}
	smallWindow := &provider.ChatRequest{
		Model: "tiny-model",
		Messages: []provider.Message{
			{Role: "user", Content: pad(80)},
			{Role: "assistant", Content: pad(80)},
			{Role: "user", Content: "tail"},
		},
	}

	if res := c.Compress(bigWindow); res.Compressed {
		t.Fatalf("big-window model should not trigger compression: %+v", res)
	}
	if res := c.Compress(smallWindow); !res.Compressed {
		t.Fatalf("small-window model should trigger compression: %+v", res)
	}
}

func TestSlidingWindow_NilOrEmptyRequest(t *testing.T) {
	c := NewSlidingWindow(SlidingWindowOptions{Tokenizer: newTestSelector(t)})
	if res := c.Compress(nil); res.Compressed {
		t.Fatal("nil request should be no-op")
	}
	if res := c.Compress(&provider.ChatRequest{Model: "x"}); res.Compressed {
		t.Fatal("empty messages should be no-op")
	}
}

func TestSlidingWindow_NilReceiverNoOp(t *testing.T) {
	var c *SlidingWindow
	res := c.Compress(&provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	if res.Compressed || res.TokensBefore != 0 {
		t.Fatalf("nil receiver should be a no-op, got %+v", res)
	}
}

func TestSlidingWindow_DefaultsApplied(t *testing.T) {
	c := NewSlidingWindow(SlidingWindowOptions{Tokenizer: newTestSelector(t)})
	if c.defaultWindow != defaultWindowFallback {
		t.Errorf("default window not applied: got %d", c.defaultWindow)
	}
	if c.triggerRatio != defaultTriggerRatio {
		t.Errorf("default trigger ratio not applied: got %v", c.triggerRatio)
	}
	if c.minKeepMessages != defaultMinKeepMessages {
		t.Errorf("default min keep not applied: got %d", c.minKeepMessages)
	}
}

func roles(msgs []provider.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}
