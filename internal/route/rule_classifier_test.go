package route

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
)

func newTestTokenizer(t *testing.T) *tokenizer.Selector {
	t.Helper()
	tk, err := tokenizer.NewSelector()
	require.NoError(t, err)
	return tk
}

func reqWithUser(model, content string) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model: model,
		Messages: []provider.Message{
			{Role: "user", Content: content},
		},
	}
}

func TestNewRuleClassifier_Validation(t *testing.T) {
	cases := []struct {
		name    string
		rules   []Rule
		wantErr string
	}{
		{
			name:    "empty Name",
			rules:   []Rule{{Name: "", RouteTo: "x"}},
			wantErr: "missing Name",
		},
		{
			name:    "empty RouteTo",
			rules:   []Rule{{Name: "r", RouteTo: ""}},
			wantErr: "missing RouteTo",
		},
		{
			name: "duplicate Name",
			rules: []Rule{
				{Name: "r", RouteTo: "a"},
				{Name: "r", RouteTo: "b"},
			},
			wantErr: "duplicate rule name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRuleClassifier(tc.rules, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestRuleClassifier_NoRules_ReturnsEmpty(t *testing.T) {
	c, err := NewRuleClassifier(nil, nil)
	require.NoError(t, err)
	d := c.Classify(reqWithUser("gpt-4o", "anything"))
	assert.True(t, d.Empty())
}

func TestRuleClassifier_NilRequest_ReturnsEmpty(t *testing.T) {
	c, err := NewRuleClassifier([]Rule{
		{Name: "always", RouteTo: "x"},
	}, nil)
	require.NoError(t, err)
	d := c.Classify(nil)
	assert.True(t, d.Empty())
}

func TestRuleClassifier_KeywordsAny_Matches(t *testing.T) {
	c, err := NewRuleClassifier([]Rule{
		{
			Name:    "translate",
			RouteTo: "gpt-4o-mini",
			When:    Condition{KeywordsAny: []string{"translate", "翻译"}},
		},
	}, nil)
	require.NoError(t, err)

	d := c.Classify(reqWithUser("gpt-4o", "Please translate this paragraph."))
	assert.Equal(t, "gpt-4o-mini", d.Model)
	assert.Equal(t, "translate", d.Rule)

	d2 := c.Classify(reqWithUser("gpt-4o", "请把这段话翻译一下"))
	assert.Equal(t, "gpt-4o-mini", d2.Model)
}

func TestRuleClassifier_KeywordsAny_CaseInsensitive(t *testing.T) {
	c, err := NewRuleClassifier([]Rule{
		{Name: "r", RouteTo: "x", When: Condition{KeywordsAny: []string{"DEBUG"}}},
	}, nil)
	require.NoError(t, err)

	d := c.Classify(reqWithUser("m", "i need to debug this thing"))
	assert.Equal(t, "x", d.Model)
}

func TestRuleClassifier_KeywordsAny_NoMatch_FallsThrough(t *testing.T) {
	c, err := NewRuleClassifier([]Rule{
		{Name: "translate", RouteTo: "cheap", When: Condition{KeywordsAny: []string{"translate"}}},
		{Name: "default", RouteTo: "default-model"},
	}, nil)
	require.NoError(t, err)

	d := c.Classify(reqWithUser("m", "explain quantum entanglement"))
	assert.Equal(t, "default-model", d.Model)
	assert.Equal(t, "default", d.Rule)
}

func TestRuleClassifier_KeywordsNone_Excludes(t *testing.T) {
	c, err := NewRuleClassifier([]Rule{
		{
			Name:    "cheap-default",
			RouteTo: "cheap",
			When:    Condition{KeywordsNone: []string{"debug", "explain"}},
		},
	}, nil)
	require.NoError(t, err)

	// "translate this" lacks any forbidden keyword → matches.
	d := c.Classify(reqWithUser("m", "translate this please"))
	assert.Equal(t, "cheap", d.Model)

	// "debug this" has a forbidden keyword → falls through.
	d2 := c.Classify(reqWithUser("m", "debug this issue"))
	assert.True(t, d2.Empty())
}

func TestRuleClassifier_TokenLimits(t *testing.T) {
	tk := newTestTokenizer(t)
	c, err := NewRuleClassifier([]Rule{
		{Name: "short", RouteTo: "cheap", When: Condition{MaxTokens: 10}},
		{Name: "long", RouteTo: "expensive", When: Condition{MinTokens: 50}},
		{Name: "default", RouteTo: "standard"},
	}, tk)
	require.NoError(t, err)

	short := c.Classify(reqWithUser("gpt-4o", "hi"))
	assert.Equal(t, "cheap", short.Model)
	assert.Equal(t, "short", short.Rule)

	// Construct a request whose prompt-tokens exceed 50 by repeating
	// content. tokenizer.cl100k counts ~1 token per ASCII word.
	longContent := ""
	for i := 0; i < 100; i++ {
		longContent += "word "
	}
	long := c.Classify(reqWithUser("gpt-4o", longContent))
	assert.Equal(t, "expensive", long.Model)
	assert.Equal(t, "long", long.Rule)

	mid := c.Classify(reqWithUser("gpt-4o", "this is a moderate length question with twelve words exactly here"))
	assert.Equal(t, "standard", mid.Model, "moderate-length req falls to default rule")
}

func TestRuleClassifier_FirstMatchWins(t *testing.T) {
	c, err := NewRuleClassifier([]Rule{
		{Name: "first", RouteTo: "model-a", When: Condition{KeywordsAny: []string{"x"}}},
		{Name: "second", RouteTo: "model-b", When: Condition{KeywordsAny: []string{"x"}}},
	}, nil)
	require.NoError(t, err)
	d := c.Classify(reqWithUser("m", "x x x"))
	assert.Equal(t, "first", d.Rule)
	assert.Equal(t, "model-a", d.Model)
}

func TestRuleClassifier_AndJoinedConditions(t *testing.T) {
	tk := newTestTokenizer(t)
	c, err := NewRuleClassifier([]Rule{
		{
			Name:    "short-translate",
			RouteTo: "cheap",
			When: Condition{
				MaxTokens:   20,
				KeywordsAny: []string{"translate"},
			},
		},
	}, tk)
	require.NoError(t, err)

	// Short + has keyword → match.
	d := c.Classify(reqWithUser("gpt-4o", "translate hi"))
	assert.Equal(t, "cheap", d.Model)

	// Short but no keyword → no match.
	d = c.Classify(reqWithUser("gpt-4o", "hello there"))
	assert.True(t, d.Empty())

	// Has keyword but too long → no match.
	long := "translate "
	for i := 0; i < 50; i++ {
		long += "extra word "
	}
	d = c.Classify(reqWithUser("gpt-4o", long))
	assert.True(t, d.Empty())
}

func TestRuleClassifier_OnlyUserMessagesConsidered(t *testing.T) {
	c, err := NewRuleClassifier([]Rule{
		{Name: "r", RouteTo: "x", When: Condition{KeywordsAny: []string{"translate"}}},
	}, nil)
	require.NoError(t, err)

	// "translate" appears only in system message — must NOT match
	// (defends against system-prompt-injected routing keywords).
	d := c.Classify(&provider.ChatRequest{
		Model: "m",
		Messages: []provider.Message{
			{Role: "system", Content: "you are a translate assistant"},
			{Role: "user", Content: "tell me a joke"},
		},
	})
	assert.True(t, d.Empty())
}

func TestRuleClassifier_NilTokenizer_TokenRulesNeverMatch(t *testing.T) {
	// Without a tokenizer, MaxTokens/MinTokens checks see count=0.
	// MaxTokens=10 is satisfied by 0 (the rule matches every request);
	// MinTokens=10 is NEVER satisfied (the rule never matches).
	c, err := NewRuleClassifier([]Rule{
		{Name: "must-be-long", RouteTo: "x", When: Condition{MinTokens: 10}},
		{Name: "fallback", RouteTo: "default"},
	}, nil)
	require.NoError(t, err)

	d := c.Classify(reqWithUser("m", "anything"))
	assert.Equal(t, "fallback", d.Rule, "MinTokens rule must NOT match when tokenizer absent")
}

func TestRuleClassifier_EmptyKeywordInListIsIgnored(t *testing.T) {
	c, err := NewRuleClassifier([]Rule{
		{Name: "r", RouteTo: "x", When: Condition{KeywordsAny: []string{"", "translate"}}},
	}, nil)
	require.NoError(t, err)

	// Empty string would match everything if not filtered — assert
	// the actual keyword "translate" is what controls.
	d := c.Classify(reqWithUser("m", "no relevant words"))
	assert.True(t, d.Empty(), "empty keyword in list must not match every request")
}

func TestDecision_Empty(t *testing.T) {
	assert.True(t, Decision{}.Empty())
	assert.False(t, Decision{Model: "x"}.Empty())
}
