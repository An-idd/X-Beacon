package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptrFloat(v float64) *float64 { return &v }

func TestChatRequest_MarshalRoundTrip(t *testing.T) {
	original := ChatRequest{
		Model:       "gpt-4o-mini",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		MaxTokens:   100,
		Temperature: ptrFloat(0.7),
		Stream:      true,
		Stop:        []string{"\n\n"},
	}
	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded ChatRequest
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, original.Model, decoded.Model)
	assert.Equal(t, original.Messages, decoded.Messages)
	assert.Equal(t, original.MaxTokens, decoded.MaxTokens)
	require.NotNil(t, decoded.Temperature)
	assert.InDelta(t, 0.7, *decoded.Temperature, 1e-9)
	assert.Equal(t, original.Stream, decoded.Stream)
	assert.Equal(t, original.Stop, decoded.Stop)
	assert.Nil(t, decoded.Extra)
}

func TestChatRequest_TemperatureZeroPreserved(t *testing.T) {
	// Temperature=0 is semantically meaningful (deterministic); the pointer
	// shape must preserve it across a round trip.
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "u"}}, Temperature: ptrFloat(0)}
	data, err := json.Marshal(req)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"temperature":0`)

	var decoded ChatRequest
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NotNil(t, decoded.Temperature)
	assert.Equal(t, 0.0, *decoded.Temperature)
}

func TestChatRequest_ExtraPassthrough(t *testing.T) {
	input := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role":"user","content":"hi"}],
		"logit_bias": {"50256": -100},
		"response_format": {"type":"json_object"}
	}`)
	var req ChatRequest
	require.NoError(t, json.Unmarshal(input, &req))

	assert.Equal(t, "gpt-4o", req.Model)
	require.NotNil(t, req.Extra)
	assert.Contains(t, req.Extra, "logit_bias")
	assert.Contains(t, req.Extra, "response_format")
	// Known keys must NOT leak into Extra.
	assert.NotContains(t, req.Extra, "model")
	assert.NotContains(t, req.Extra, "messages")

	out, err := json.Marshal(req)
	require.NoError(t, err)

	var reparsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &reparsed))
	assert.Contains(t, reparsed, "logit_bias")
	assert.Contains(t, reparsed, "response_format")
	assert.Contains(t, reparsed, "model")
	assert.Contains(t, reparsed, "messages")
}

func TestChatRequest_ExtraCannotShadowKnown(t *testing.T) {
	// Even if Extra somehow contains a known key (defensive: shouldn't
	// happen since UnmarshalJSON strips them), the struct fields must win
	// on marshal.
	req := ChatRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"model":        json.RawMessage(`"gpt-EVIL"`),
			"custom_field": json.RawMessage(`"preserved"`),
		},
	}
	out, err := json.Marshal(req)
	require.NoError(t, err)

	var reparsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &reparsed))
	assert.JSONEq(t, `"gpt-4o"`, string(reparsed["model"]))
	assert.JSONEq(t, `"preserved"`, string(reparsed["custom_field"]))
}

func TestChatRequest_String_NoPromptLeak(t *testing.T) {
	req := &ChatRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "my secret password is hunter2"}},
		Stream:   true,
	}
	s := req.String()
	assert.Contains(t, s, "gpt-4o")
	assert.Contains(t, s, "messages=1")
	assert.NotContains(t, s, "hunter2")
	assert.NotContains(t, s, "password")
}

func TestStreamEvent_ZeroValue(t *testing.T) {
	var ev StreamEvent
	assert.Nil(t, ev.Chunk)
	assert.NoError(t, ev.Err)
}
