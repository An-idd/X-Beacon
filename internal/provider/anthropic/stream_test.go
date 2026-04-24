package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

// --- Real-shape SSE frames borrowed from Anthropic API reference ---

const (
	evMessageStart = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01ABC","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet-20241022","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":1}}}`

	evContentBlockStart = `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`

	evContentBlockDeltaHello = `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`

	evContentBlockDeltaWorld = `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`

	evContentBlockDeltaToolUse = `event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"k\":"}}`

	evContentBlockStop = `event: content_block_stop
data: {"type":"content_block_stop","index":0}`

	evMessageDelta = `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`

	evMessageStop = `event: message_stop
data: {"type":"message_stop"}`

	evPing = `event: ping
data: {"type":"ping"}`
)

func sseFrames(events ...string) string {
	var sb strings.Builder
	for _, e := range events {
		sb.WriteString(e)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

func writeFlushing(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	_, _ = io.WriteString(w, s)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func streamRequest() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "claude-3-5-sonnet-20241022",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
}

// --- Happy path ---

func TestStream_HappyPath_MessageStartToStop(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		// Verify stream=true hit the wire.
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), `"stream":true`)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFlushing(t, w, sseFrames(
			evMessageStart,
			evContentBlockStart,
			evContentBlockDeltaHello,
			evContentBlockDeltaWorld,
			evContentBlockStop,
			evMessageDelta,
			evMessageStop,
		))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var chunks []*provider.ChatStreamChunk
	for ev := range ch {
		require.NoError(t, ev.Err)
		require.NotNil(t, ev.Chunk)
		chunks = append(chunks, ev.Chunk)
	}

	// Expected: 1 role chunk + 2 content deltas + 1 finish chunk = 4 chunks.
	require.Len(t, chunks, 4)

	// Chunk 1: role-only delta from message_start.
	assert.Equal(t, "msg_01ABC", chunks[0].ID)
	assert.Equal(t, "claude-3-5-sonnet-20241022", chunks[0].Model)
	assert.Equal(t, "assistant", chunks[0].Choices[0].Delta.Role)
	assert.Empty(t, chunks[0].Choices[0].Delta.Content)

	// Chunk 2 & 3: content deltas.
	assert.Equal(t, "Hello", chunks[1].Choices[0].Delta.Content)
	assert.Equal(t, " world", chunks[2].Choices[0].Delta.Content)

	// Chunk 4: finish + usage.
	assert.Equal(t, "stop", chunks[3].Choices[0].FinishReason)
	require.NotNil(t, chunks[3].Usage)
	assert.Equal(t, 25, chunks[3].Usage.PromptTokens)
	assert.Equal(t, 15, chunks[3].Usage.CompletionTokens)
	assert.Equal(t, 40, chunks[3].Usage.TotalTokens)
}

func TestStream_FinalChunkCarriesUsage(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeFlushing(t, w, sseFrames(evMessageStart, evContentBlockDeltaHello, evMessageDelta, evMessageStop))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var last *provider.ChatStreamChunk
	for ev := range ch {
		require.NoError(t, ev.Err)
		last = ev.Chunk
	}
	require.NotNil(t, last)
	require.NotNil(t, last.Usage)
	assert.Equal(t, 25, last.Usage.PromptTokens)
	assert.Equal(t, 15, last.Usage.CompletionTokens)
	assert.NotEmpty(t, last.Choices[0].FinishReason)
}

// --- Events that should be silently ignored ---

func TestStream_PingIgnored(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeFlushing(t, w, sseFrames(
			evMessageStart,
			evPing,
			evContentBlockDeltaHello,
			evPing,
			evMessageDelta,
			evMessageStop,
		))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var chunks int
	for ev := range ch {
		require.NoError(t, ev.Err)
		chunks++
	}
	// ping events must not produce chunks.
	assert.Equal(t, 3, chunks) // role + 1 delta + finish
}

func TestStream_ContentBlockStartStopIgnored(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeFlushing(t, w, sseFrames(
			evMessageStart,
			evContentBlockStart,
			evContentBlockStart, // duplicate allowed, should still no-op
			evContentBlockStop,
			evContentBlockStop,
			evMessageDelta,
			evMessageStop,
		))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var chunks int
	for ev := range ch {
		require.NoError(t, ev.Err)
		chunks++
	}
	// start/stop events produce no chunks.
	assert.Equal(t, 2, chunks) // role + finish
}

func TestStream_InputJsonDeltaIgnored(t *testing.T) {
	// tool_use-style deltas must not surface as text content.
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeFlushing(t, w, sseFrames(
			evMessageStart,
			evContentBlockDeltaHello,
			evContentBlockDeltaToolUse, // should be dropped
			evMessageDelta,
			evMessageStop,
		))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var contents []string
	for ev := range ch {
		require.NoError(t, ev.Err)
		if c := ev.Chunk.Choices[0].Delta.Content; c != "" {
			contents = append(contents, c)
		}
	}
	// Only the text_delta contributed content; the input_json_delta did not.
	assert.Equal(t, []string{"Hello"}, contents)
}

func TestStream_UnknownEventTypeIgnored(t *testing.T) {
	// Forward-compat: unknown events don't break the stream.
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeFlushing(t, w, sseFrames(
			evMessageStart,
			`event: future_event
data: {"type":"future_event","surprise":"hi"}`,
			evContentBlockDeltaHello,
			evMessageStop,
		))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var chunks int
	for ev := range ch {
		require.NoError(t, ev.Err)
		chunks++
	}
	assert.Equal(t, 2, chunks) // role + text_delta (no finish chunk since no message_delta)
}

// --- Error paths ---

func TestStream_UpstreamErrorAtStart_401(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.Error(t, err)
	assert.Nil(t, ch)
	assert.True(t, errors.Is(err, provider.ErrAuth))

	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.Equal(t, 401, ue.StatusCode)
}

func TestStream_MidStreamErrorEvent(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeFlushing(t, w, sseFrames(
			evMessageStart,
			evContentBlockDeltaHello,
			`event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"temporarily overloaded"}}`,
		))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var chunks int
	var sawErr bool
	for ev := range ch {
		if ev.Err != nil {
			sawErr = true
			assert.True(t, errors.Is(ev.Err, provider.ErrUnavailable))
			assert.Contains(t, ev.Err.Error(), "temporarily overloaded")
			continue
		}
		chunks++
	}
	assert.Equal(t, 2, chunks, "role + one text delta before the error")
	assert.True(t, sawErr)
}

func TestStream_MalformedData(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeFlushing(t, w, sseFrames(
			evMessageStart,
			`event: content_block_delta
data: this-is-not-json`,
			evMessageStop,
		))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var sawChunk, sawErr bool
	for ev := range ch {
		if ev.Err != nil {
			sawErr = true
			assert.True(t, errors.Is(ev.Err, provider.ErrUpstream))
			assert.Contains(t, ev.Err.Error(), "decode event")
			continue
		}
		sawChunk = true
	}
	assert.True(t, sawChunk, "message_start should have produced the role chunk")
	assert.True(t, sawErr)
}

func TestStream_EOFWithoutMessageStop(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Deliberately skip message_stop.
		writeFlushing(t, w, sseFrames(evMessageStart, evContentBlockDeltaHello))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var chunks int
	var lastErr error
	for ev := range ch {
		if ev.Err != nil {
			lastErr = ev.Err
			continue
		}
		chunks++
	}
	assert.Equal(t, 2, chunks)
	require.Error(t, lastErr)
	assert.True(t, errors.Is(lastErr, provider.ErrUpstream))
	assert.Contains(t, lastErr.Error(), "message_stop")
}

func TestStream_PreCancelledCtx(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run with pre-cancelled ctx")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := p.ChatCompletionStream(ctx, streamRequest())
	require.Error(t, err)
	assert.Nil(t, ch)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestStream_ClientCancels(t *testing.T) {
	serverCtx, serverCancel := context.WithCancel(context.Background())
	t.Cleanup(serverCancel)
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeFlushing(t, w, sseFrames(evMessageStart, evContentBlockDeltaHello))
		// Hold until either client or test cleanup disconnects us.
		for {
			select {
			case <-r.Context().Done():
				return
			case <-serverCtx.Done():
				return
			case <-time.After(50 * time.Millisecond):
				writeFlushing(t, w, sseFrames(evContentBlockDeltaWorld))
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.ChatCompletionStream(ctx, streamRequest())
	require.NoError(t, err)

	// Drain at least one chunk, then cancel.
	ev, ok := <-ch
	require.True(t, ok)
	require.NoError(t, ev.Err)
	cancel()

	// Channel must close within a reasonable window (proves goroutine exit).
	select {
	case _, stillOpen := <-ch:
		if stillOpen {
			for range ch {
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream channel did not close after client cancel")
	}
}

func TestStream_NilRequest(t *testing.T) {
	p, _ := New(Config{Name: "x", APIKey: "sk-x"})
	ch, err := p.ChatCompletionStream(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, ch)
	assert.True(t, errors.Is(err, provider.ErrInvalidRequest))
}
