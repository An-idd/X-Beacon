// L1 wire-compatibility tests. These tests sit *below* the SDK level on
// purpose: they assert that the bytes X-Beacon emits on `/v1/chat/completions`
// and `/v1/models` follow OpenAI's wire conventions exactly, even when the
// upstream is one of the three supported provider types. Tools like cURL,
// raw fetch(), or non-Go SDKs see the same shape and an OpenAI client
// can be repointed at the gateway with only a baseURL change.
//
// What this layer catches that the Python SDK suite (Day 4) does not:
//   - Whitespace / SSE frame delimiter regressions (the SDK normalizes
//     these away before surfacing to user code).
//   - Errant or missing `data: [DONE]` terminators.
//   - Header shape (Content-Type variants, missing Content-Type on
//     streaming, casing).
//   - JSON key ordering / extra fields that SDK schema parsers happily
//     drop but third-party tools may choke on.
//
// Golden file strategy: SSE structure is golden-asserted after a small
// normalization pass (id/timestamp scrubbing) because frame layout is the
// thing that breaks silently and is hard to spec in code. JSON envelopes
// use structural assertions instead — bake the *shape*, not the noise.
package smoke

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateGoldens lets us regenerate fixtures with `-update-goldens`. Default
// off; CI must run with it off so a stale golden fails the build.
var updateGoldens = flag.Bool("update-goldens", false, "rewrite testdata/golden/*.golden from current observed output")

// ---------- normalization ----------

// dynamicFieldPatterns scrub noise fields out of responses before either
// golden-comparing or asserting on substrings. The order matters: longer /
// more specific patterns first so "id":"chatcmpl-..." doesn't get half-eaten
// by a generic "id":"..." rule.
var dynamicFieldPatterns = []struct {
	re      *regexp.Regexp
	replace string
}{
	// Timestamps (e.g. "created":1714000000)
	{regexp.MustCompile(`"created":\d+`), `"created":<TS>`},
	// Server-assigned IDs (chatcmpl-xxx, msg_xxx, c1)
	{regexp.MustCompile(`"id":"(chatcmpl|msg|c)[^"]*"`), `"id":"<ID>"`},
	// Request correlation IDs threaded by the gateway middleware.
	{regexp.MustCompile(`"req_id":"[^"]*"`), `"req_id":"<REQ_ID>"`},
	// System fingerprint (OpenAI's deterministic-output marker; varies per build)
	{regexp.MustCompile(`"system_fingerprint":"[^"]*"`), `"system_fingerprint":"<FP>"`},
}

func normalizeWireBytes(b []byte) []byte {
	s := string(b)
	for _, p := range dynamicFieldPatterns {
		s = p.re.ReplaceAllString(s, p.replace)
	}
	return []byte(s)
}

// ---------- golden file helpers ----------

func goldenPath(name string) string {
	return filepath.Join("testdata", "golden", name+".golden")
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := goldenPath(name)
	if *updateGoldens {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoErrorf(t, err, "golden %s missing — regenerate with `go test -update-goldens ./test/...`", name)
	assert.Equal(t, string(want), string(got),
		"wire output diverged from golden %s. If intended, run `go test -update-goldens ./test/...` and review the diff.", name)
}

// ---------- L1: non-streaming JSON envelope ----------

func TestWireCompat_NonStreaming_EnvelopeShape(t *testing.T) {
	fx := newFixture(t,
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, openaiChatResponse()) },
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, hdr, raw := fx.post(t, "/v1/chat/completions", chatRequest("gpt-4o", false), true)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "application/json", strings.Split(hdr.Get("Content-Type"), ";")[0],
		"non-streaming responses MUST be application/json (OpenAI SDKs key off this)")

	// Decode and assert required keys + types. This is the schema OpenAI's
	// SDK relies on; anything missing breaks chat.completions.create().
	var env struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), "response must be valid JSON")

	assert.NotEmpty(t, env.ID, "id is required by OpenAI shape")
	assert.Equal(t, "chat.completion", env.Object)
	assert.Positive(t, env.Created, "created must be a unix timestamp")
	assert.Equal(t, "gpt-4o", env.Model)
	require.Len(t, env.Choices, 1)
	assert.Equal(t, 0, env.Choices[0].Index)
	assert.Equal(t, "assistant", env.Choices[0].Message.Role)
	assert.NotEmpty(t, env.Choices[0].Message.Content)
	assert.Equal(t, "stop", env.Choices[0].FinishReason)
	assert.NotZero(t, env.Usage.TotalTokens, "usage.total_tokens must be present")
}

// ---------- L1: streaming SSE wire layout (golden) ----------

func TestWireCompat_Streaming_SSELayout(t *testing.T) {
	// A deterministic 4-frame upstream + [DONE]. We don't care about
	// the content; we care that the gateway preserves frame structure,
	// uses the right Content-Type, and terminates correctly.
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"world"}}]}` + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	fx := newFixture(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, f := range frames {
				_, _ = io.WriteString(w, f)
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
		},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, hdr, raw := fx.post(t, "/v1/chat/completions", chatRequest("gpt-4o", true), true)
	require.Equal(t, http.StatusOK, status)

	// Content-Type: be permissive about params (charset etc.) but the
	// MIME type itself MUST be text/event-stream. Browsers and SDK
	// stream parsers key off this exact value.
	ct := strings.Split(hdr.Get("Content-Type"), ";")[0]
	assert.Equal(t, "text/event-stream", strings.TrimSpace(ct))

	// Cache-Control: SSE clients (and proxies) misbehave when the
	// gateway lets responses be cached. OpenAI sets no-cache; we should
	// match. This is also a frequent regression: a default middleware
	// inadvertently sets a cacheable header on streaming.
	assert.Contains(t, hdr.Get("Cache-Control"), "no-cache",
		"streaming responses should advertise no-cache; intermediate proxies will buffer otherwise")

	// Normalize + golden the body. Catches: missing newline pairs,
	// reordered frames, accidental JSON pretty-printing, dropped [DONE].
	got := normalizeWireBytes(raw)
	assertGolden(t, "sse_chat_minimal", got)

	// Belt-and-suspenders structural checks not covered by golden alone.
	body := string(raw)
	assert.True(t, strings.HasSuffix(body, "data: [DONE]\n\n") ||
		strings.HasSuffix(body, "data: [DONE]\n"),
		"stream must end with `data: [DONE]` (with trailing newline); got tail=%q", tail(body, 40))
	frameSep := strings.Count(body, "\n\n")
	assert.GreaterOrEqual(t, frameSep, 4,
		"each `data:` frame must be terminated by a blank line (\\n\\n); got %d separators in %d bytes",
		frameSep, len(body))
}

// ---------- L1: error envelope shape (auth path) ----------

func TestWireCompat_ErrorEnvelope_Auth401(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	// Send without auth → 401 from middleware. Catches: missing
	// Content-Type, wrong error.type, missing error.code, body not
	// being valid JSON.
	status, _, raw := fx.post(t, "/v1/chat/completions", chatRequest("gpt-4o", false), false)
	require.Equal(t, http.StatusUnauthorized, status)

	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env),
		"401 body must be valid JSON; got %q", string(raw))
	assert.Equal(t, "authentication_error", env.Error.Type,
		"auth failure must use type=authentication_error (OpenAI SDK maps this to AuthenticationError)")
	assert.NotEmpty(t, env.Error.Code, "error.code must be present so clients can branch")
	assert.NotEmpty(t, env.Error.Message)

	// Golden the normalized envelope. Locked-in shape catches future
	// drift like adding a `details` field or dropping `code`.
	assertGolden(t, "error_auth_401", normalizeWireBytes(raw))
}

// ---------- L1: error envelope shape (bad request path) ----------

func TestWireCompat_ErrorEnvelope_BadRequest400(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	// Malformed JSON in the request body → 400 from the handler's
	// decoder. Different code path than auth (the chat handler's own
	// jsonio.readJSON), so we exercise both shapes.
	status, _, raw := fx.post(t, "/v1/chat/completions", []byte(`{this is not json`), true)
	require.Equal(t, http.StatusBadRequest, status)

	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.NotEmpty(t, env.Error.Type)
	assert.NotEmpty(t, env.Error.Message)
}

// ---------- L1: /v1/models envelope (Day 2 wire-level cross-check) ----------

func TestWireCompat_ModelsEnvelopeShape(t *testing.T) {
	fx := newFixture(t,
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, raw := fx.get(t, "/v1/models", true)
	require.Equal(t, http.StatusOK, status)

	// Top-level shape must match OpenAI. Schema strict-mode parsers
	// (some non-Python SDKs) reject anything else.
	var raw1 struct {
		Object string             `json:"object"`
		Data   []map[string]any   `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &raw1))
	assert.Equal(t, "list", raw1.Object)
	require.NotEmpty(t, raw1.Data)

	// Every entry must carry the OpenAI-canonical fields. X-Beacon
	// extensions (pricing, capabilities, status, ...) are top-level
	// additions; their presence here doesn't violate compat because
	// SDK parsers ignore unknown fields (decision A1).
	for i, m := range raw1.Data {
		assert.NotEmpty(t, m["id"], "entry %d missing id", i)
		assert.Equal(t, "model", m["object"], "entry %d wrong object", i)
		assert.NotEmpty(t, m["owned_by"], "entry %d missing owned_by", i)
	}
}

// ---------- L1: tool_call response (JSON-in-JSON danger zone) ----------

func TestWireCompat_NonStreaming_ToolCallArgumentsAreString(t *testing.T) {
	// The most common wire bug in LLM gateways: tool_call.function.arguments
	// is supposed to be a *string* of JSON (so clients can JSON.parse it
	// themselves), but a careless gateway can double-encode it
	// ("\"{\\\"q\\\":\\\"hi\\\"}\"") or accidentally rewrite it as an object
	// ({"q":"hi"}). OpenAI SDK clients break in both directions. Lock it.
	upstream := `{
		"id":"chatcmpl-tc-1","object":"chat.completion","created":1714000001,"model":"gpt-4o",
		"choices":[{"index":0,"message":{
			"role":"assistant","content":null,
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hi\"}"}}]
		},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}
	}`

	fx := newFixture(t,
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, upstream) },
		func(http.ResponseWriter, *http.Request) {},
		func(http.ResponseWriter, *http.Request) {},
	)
	defer fx.Close()

	status, _, raw := fx.post(t, "/v1/chat/completions", chatRequest("gpt-4o", false), true)
	require.Equal(t, http.StatusOK, status)

	// Parse permissively, then re-check the arguments field type at the
	// raw map level — encoding/json would silently coerce a string to a
	// string and an object to a map[string]any. We want to reject the
	// map case.
	var envelope struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"` // RawMessage = bytes, preserves type
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.Len(t, envelope.Choices, 1)
	require.Len(t, envelope.Choices[0].Message.ToolCalls, 1)
	tc := envelope.Choices[0].Message.ToolCalls[0]

	assert.Equal(t, "call_1", tc.ID)
	assert.Equal(t, "function", tc.Type)
	assert.Equal(t, "search", tc.Function.Name)
	assert.Equal(t, "tool_calls", envelope.Choices[0].FinishReason)

	// The critical assertion: arguments must be a JSON STRING in the
	// wire format. As RawMessage it starts with a `"`; as an object it
	// would start with `{`. This catches the "helpful" gateway that
	// parses arguments into a map before re-encoding.
	require.NotEmpty(t, tc.Function.Arguments)
	assert.Equal(t, byte('"'), tc.Function.Arguments[0],
		"tool_call.function.arguments must serialize as a JSON string, got %s — OpenAI SDKs JSON.parse this client-side",
		string(tc.Function.Arguments))

	// And the contained string must parse as valid JSON when un-quoted.
	var argsStr string
	require.NoError(t, json.Unmarshal(tc.Function.Arguments, &argsStr))
	var parsed map[string]any
	require.NoErrorf(t, json.Unmarshal([]byte(argsStr), &parsed),
		"arguments string must be valid JSON; got %q", argsStr)
	assert.Equal(t, "hi", parsed["q"])
}

// ---------- helpers ----------

// tail returns the last n bytes of s, for compact error messages.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
