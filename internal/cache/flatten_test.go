package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/An-idd/x-beacon/internal/provider"
)

func TestFlattenForEmbedding(t *testing.T) {
	cases := []struct {
		name     string
		req      *provider.ChatRequest
		expected string
	}{
		{
			name:     "nil request",
			req:      nil,
			expected: "",
		},
		{
			name: "user only",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "user", Content: "hello"},
				},
			},
			expected: "hello",
		},
		{
			name: "system + user",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "system", Content: "you are a python tutor"},
					{Role: "user", Content: "what is a list comprehension?"},
				},
			},
			expected: "you are a python tutor\n\nwhat is a list comprehension?",
		},
		{
			name: "multiple system messages joined",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "system", Content: "you are concise"},
					{Role: "system", Content: "answer in markdown"},
					{Role: "user", Content: "explain pointers"},
				},
			},
			expected: "you are concise\n\nanswer in markdown\n\nexplain pointers",
		},
		{
			name: "only the last user message wins",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "user", Content: "first"},
					{Role: "assistant", Content: "ok"},
					{Role: "user", Content: "second"},
				},
			},
			expected: "second",
		},
		{
			name: "assistant turns ignored entirely",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "system", Content: "be helpful"},
					{Role: "user", Content: "step 1"},
					{Role: "assistant", Content: "doing step 1..."},
					{Role: "user", Content: "step 2"},
					{Role: "assistant", Content: "doing step 2..."},
				},
			},
			expected: "be helpful\n\nstep 2",
		},
		{
			name: "tool / function messages ignored",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "user", Content: "call the api"},
					{Role: "tool", Content: "{\"result\":42}", ToolCallID: "abc"},
				},
			},
			expected: "call the api",
		},
		{
			name: "empty content turns skipped",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "system", Content: "  "},
					{Role: "system", Content: "actual system"},
					{Role: "user", Content: ""},
					{Role: "user", Content: "real question"},
				},
			},
			expected: "actual system\n\nreal question",
		},
		{
			name: "leading and trailing whitespace trimmed",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "user", Content: "   what time is it?\n   "},
				},
			},
			expected: "what time is it?",
		},
		{
			name: "no user message yields empty",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "system", Content: "be helpful"},
					{Role: "assistant", Content: "anyone there?"},
				},
			},
			expected: "",
		},
		{
			name:     "empty messages slice",
			req:      &provider.ChatRequest{Messages: nil},
			expected: "",
		},
		{
			name: "case is preserved",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "user", Content: "Hello WORLD"},
				},
			},
			expected: "Hello WORLD",
		},
		{
			name: "punctuation is preserved",
			req: &provider.ChatRequest{
				Messages: []provider.Message{
					{Role: "user", Content: "stop."},
				},
			},
			expected: "stop.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, FlattenForEmbedding(tc.req))
		})
	}
}

// Same logical request must flatten deterministically — guards against
// accidental map iteration sneaking in.
func TestFlattenForEmbedding_Deterministic(t *testing.T) {
	req := &provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "system", Content: "a"},
			{Role: "system", Content: "b"},
			{Role: "system", Content: "c"},
			{Role: "user", Content: "q"},
		},
	}
	first := FlattenForEmbedding(req)
	for i := 0; i < 100; i++ {
		assert.Equal(t, first, FlattenForEmbedding(req))
	}
}
