package tokenizer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

func TestOpenAI_CountText_Empty(t *testing.T) {
	tk, err := NewOpenAI()
	require.NoError(t, err)
	assert.Equal(t, 0, tk.CountText(""))
}

func TestOpenAI_CountText_KnownStrings(t *testing.T) {
	// Counts come from OpenAI's official tiktoken (cl100k_base) and are
	// stable across releases — pinning here lets us catch silent vocab
	// drift if we ever swap tokenizer libraries.
	tk, err := NewOpenAI()
	require.NoError(t, err)

	cases := []struct {
		text     string
		expected int
	}{
		{"hello", 1},
		{"hello world", 2},
		{"the quick brown fox jumps over the lazy dog", 9},
		{strings.Repeat("a", 100), 13}, // dense ASCII compresses well in BPE
	}
	for _, tc := range cases {
		got := tk.CountText(tc.text)
		assert.Equal(t, tc.expected, got, "text=%q", tc.text)
	}
}

func TestOpenAI_CountMessages_OverheadApplied(t *testing.T) {
	tk, err := NewOpenAI()
	require.NoError(t, err)

	one := tk.CountMessages([]provider.Message{{Role: "user", Content: "hi"}})
	two := tk.CountMessages([]provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	})
	// Two messages should add at least one perMessageOverhead beyond one
	// message; the priming overhead is paid once. Each message also adds
	// role + content tokens. Lower bound: more than just text counts.
	assert.Greater(t, two, one)
	assert.GreaterOrEqual(t, one, primingOverhead+perMessageOverhead+1)
}

func TestOpenAI_CountMessages_Empty(t *testing.T) {
	tk, err := NewOpenAI()
	require.NoError(t, err)
	assert.Equal(t, 0, tk.CountMessages(nil))
	assert.Equal(t, 0, tk.CountMessages([]provider.Message{}))
}

func TestOpenAI_CountMessages_NameField(t *testing.T) {
	tk, err := NewOpenAI()
	require.NoError(t, err)
	without := tk.CountMessages([]provider.Message{{Role: "user", Content: "hi"}})
	withName := tk.CountMessages([]provider.Message{{Role: "user", Content: "hi", Name: "alice"}})
	assert.Greater(t, withName, without)
}

func TestOpenAI_Family(t *testing.T) {
	tk, err := NewOpenAI()
	require.NoError(t, err)
	assert.Equal(t, "openai-cl100k", tk.Family())
}

func TestAnthropic_ScalesAboveOpenAI(t *testing.T) {
	// Anthropic estimate must be ≥ OpenAI count (we scale by 1.15×).
	o, err := NewOpenAI()
	require.NoError(t, err)
	a, err := NewAnthropic()
	require.NoError(t, err)

	text := "the quick brown fox jumps over the lazy dog"
	openaiCount := o.CountText(text)
	anthCount := a.CountText(text)
	assert.GreaterOrEqual(t, anthCount, openaiCount,
		"anthropic must not under-count vs cl100k baseline")
	// And the scaling cap: never more than 1.5× (sanity bound on factor).
	assert.LessOrEqual(t, anthCount, openaiCount*3/2)
}

func TestAnthropic_ScaleNonZeroForOneToken(t *testing.T) {
	// A 1-token openai count must remain ≥ 1 anthropic, not round to 0.
	a, err := NewAnthropic()
	require.NoError(t, err)
	got := a.CountText("a")
	assert.GreaterOrEqual(t, got, 1)
}

func TestAnthropic_Family(t *testing.T) {
	a, err := NewAnthropic()
	require.NoError(t, err)
	assert.Equal(t, "anthropic-approx", a.Family())
}

func TestSelector_PicksByModel(t *testing.T) {
	sel, err := NewSelector()
	require.NoError(t, err)

	cases := map[string]string{
		"gpt-4o":             "openai-cl100k",
		"gpt-4o-mini":        "openai-cl100k",
		"deepseek-chat":      "openai-cl100k", // unknown → openai default
		"claude-3-5-sonnet":  "anthropic-approx",
		"Claude-3-Opus":      "anthropic-approx", // case-insensitive prefix
		"random-model":       "openai-cl100k",
	}
	for model, family := range cases {
		got := sel.For(model).Family()
		assert.Equal(t, family, got, "model=%q", model)
	}
}

func TestScale(t *testing.T) {
	assert.Equal(t, 0, scale(0, 1.15))
	assert.Equal(t, 1, scale(1, 1.15))   // half-up: 1.15 → 1
	assert.Equal(t, 2, scale(2, 1.15))   // 2.3 → 2
	assert.Equal(t, 5, scale(4, 1.15))   // 4.6 → 5
	assert.Equal(t, 23, scale(20, 1.15)) // 23.0
}

func TestCharEstimate(t *testing.T) {
	assert.Equal(t, 0, charEstimate(""))
	assert.Equal(t, 1, charEstimate("ab"))   // < 4 chars → 1
	assert.Equal(t, 1, charEstimate("    ")) // whitespace → empty
	assert.Equal(t, 2, charEstimate("abcdefgh"))
	assert.Equal(t, 2, charEstimate("abcd  efgh")) // whitespace stripped
}
