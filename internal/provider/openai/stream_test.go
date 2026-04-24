package openai

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

// sseFrames joins data frames with proper "\n\n" terminators.
func sseFrames(frames ...string) string {
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString(f)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// writeFlushing writes to w and flushes immediately (httptest respects
// http.Flusher). Used to simulate upstream streaming chunk-by-chunk.
func writeFlushing(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	_, _ = io.WriteString(w, s)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

const chunk1 = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`
const chunk2 = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`
const chunk3 = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`
const chunkDone = `data: [DONE]`

func streamRequest() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
}

func TestStream_Success_ThreeChunks(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify accept header and stream=true in body.
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFlushing(t, w, sseFrames(chunk1, chunk2, chunk3, chunkDone))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)
	require.NotNil(t, ch)

	var received []*provider.ChatStreamChunk
	for ev := range ch {
		require.NoError(t, ev.Err, "no error event expected in happy path")
		require.NotNil(t, ev.Chunk)
		received = append(received, ev.Chunk)
	}
	require.Len(t, received, 3)
	assert.Equal(t, "Hel", received[1].Choices[0].Delta.Content)
	assert.Equal(t, "stop", received[2].Choices[0].FinishReason)
}

func TestStream_UpstreamErrorBeforeFirstChunk(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.Error(t, err)
	assert.Nil(t, ch)
	assert.True(t, errors.Is(err, provider.ErrAuth))
	// Error is returned synchronously — no channel to drain.
	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.Equal(t, 401, ue.StatusCode)
}

func TestStream_UpstreamDisconnect(t *testing.T) {
	// Server writes two chunks and closes the body before [DONE].
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFlushing(t, w, sseFrames(chunk1, chunk2))
		// Handler returning here closes the response body.
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
	assert.Contains(t, lastErr.Error(), "[DONE]")
}

func TestStream_MalformedChunk(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// First chunk is valid; second is not JSON.
		writeFlushing(t, w, sseFrames(chunk1, "data: this-is-not-json", chunkDone))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var sawChunk bool
	var sawErr bool
	for ev := range ch {
		if ev.Err != nil {
			sawErr = true
			assert.True(t, errors.Is(ev.Err, provider.ErrUpstream))
			assert.Contains(t, ev.Err.Error(), "decode chunk")
			continue
		}
		sawChunk = true
	}
	assert.True(t, sawChunk, "first valid chunk should have been delivered")
	assert.True(t, sawErr, "malformed chunk should yield an error event")
}

func TestStream_ClientCancels(t *testing.T) {
	// Server trickles chunks forever; client cancels after receiving one.
	serverCtx, serverCancel := context.WithCancel(context.Background())
	t.Cleanup(serverCancel)
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFlushing(t, w, sseFrames(chunk1))
		for {
			select {
			case <-r.Context().Done():
				return
			case <-serverCtx.Done():
				return
			case <-time.After(50 * time.Millisecond):
				writeFlushing(t, w, sseFrames(chunk2))
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.ChatCompletionStream(ctx, streamRequest())
	require.NoError(t, err)

	// Drain one chunk, then cancel.
	ev, ok := <-ch
	require.True(t, ok)
	require.NoError(t, ev.Err)
	require.NotNil(t, ev.Chunk)
	cancel()

	// Channel must close within a reasonable window — proves the SSE
	// goroutine exited (defer close(ch)).
	select {
	case _, ok := <-ch:
		// Any remaining events drain; we don't care about their contents,
		// only that the channel ultimately closes.
		if ok {
			// drain the rest
			for range ch {
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream channel did not close after client cancel")
	}
}

func TestStream_ContextPreCancelled(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler reached despite pre-cancelled ctx")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := p.ChatCompletionStream(ctx, streamRequest())
	require.Error(t, err)
	assert.Nil(t, ch)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestStream_CommentsAndEmptyLines(t *testing.T) {
	// Upstream sends keepalive comments, blank lines, and no-space "data:"
	// variants interleaved with real chunks.
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFlushing(t, w, ": keepalive\n\n")
		writeFlushing(t, w, sseFrames(chunk1))
		writeFlushing(t, w, "\n\n") // stray blank
		writeFlushing(t, w, ": another comment\n\n")
		// No-space variant: "data:{...}".
		noSpaceChunk := strings.Replace(chunk2, "data: ", "data:", 1)
		writeFlushing(t, w, sseFrames(noSpaceChunk, chunkDone))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var chunks int
	for ev := range ch {
		require.NoError(t, ev.Err)
		require.NotNil(t, ev.Chunk)
		chunks++
	}
	assert.Equal(t, 2, chunks, "only data: chunks should surface; comments and blanks ignored")
}

func TestStream_NilRequest(t *testing.T) {
	p, _ := New(Config{Name: "openai", APIKey: "sk-x"})
	ch, err := p.ChatCompletionStream(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, ch)
	assert.True(t, errors.Is(err, provider.ErrInvalidRequest))
}

// TestStream_MidStreamErrorChunk verifies that when OpenAI sends an error
// envelope inline (after HTTP 200) — e.g. server overload partway through
// a long generation — the adapter surfaces it as a typed error event
// instead of forwarding an empty chunk to the caller.
func TestStream_MidStreamErrorChunk(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// First a legitimate delta, then an error envelope, then [DONE]
		// (which we must ignore because the error terminates the stream).
		writeFlushing(t, w, sseFrames(
			chunk1,
			`data: {"error":{"message":"server overloaded","type":"server_error","code":"server_error"}}`,
			chunkDone,
		))
	})

	ch, err := p.ChatCompletionStream(context.Background(), streamRequest())
	require.NoError(t, err)

	var chunks int
	var sawErr bool
	for ev := range ch {
		if ev.Err != nil {
			sawErr = true
			assert.Contains(t, ev.Err.Error(), "server overloaded")
			assert.True(t, errors.Is(ev.Err, provider.ErrUpstream))
			continue
		}
		require.NotNil(t, ev.Chunk)
		// Legitimate chunks before the error must still be delivered.
		assert.NotEmpty(t, ev.Chunk.ID, "chunk must not be empty (bug pre-fix would emit empty chunks here)")
		chunks++
	}
	assert.Equal(t, 1, chunks, "only the one valid chunk before the error should surface")
	assert.True(t, sawErr, "mid-stream error envelope must produce an Err event")
}
