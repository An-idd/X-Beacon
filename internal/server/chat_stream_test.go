package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushWrite emits one SSE chunk to an upstream test server and forces
// it through the response buffer so the gateway sees the chunk arrive
// in real time.
func flushWrite(t *testing.T, w http.ResponseWriter, payload string) {
	t.Helper()
	_, err := io.WriteString(w, payload)
	require.NoError(t, err)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// streamUpstreamSSE writes a sequence of pre-formatted SSE frames as the
// upstream OpenAI would. terminator controls whether [DONE] is appended.
func streamUpstreamSSE(t *testing.T, frames []string, includeDone bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			flushWrite(t, w, f)
		}
		if includeDone {
			flushWrite(t, w, "data: [DONE]\n\n")
		}
	}
}

// runStream drives a streaming /v1/chat/completions request through a
// real *http.Server (not httptest.NewRecorder, which doesn't reliably
// surface streaming semantics). Returns the full response body and code.
func runStream(t *testing.T, srv *Server, body []byte) (int, string, http.Header) {
	t.Helper()
	gateway := httptest.NewServer(srv.Handler())
	t.Cleanup(gateway.Close)

	resp, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw), resp.Header
}

func TestChatStream_HappyPath(t *testing.T) {
	upstream := streamUpstreamSSE(t,
		[]string{
			`data: {"id":"c1","object":"chat.completion.chunk","created":1714000000,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","created":1714000000,"model":"test-model","choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","created":1714000000,"model":"test-model","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}` + "\n\n",
		},
		true,
	)
	srv := newChatHandlerSrv(t, upstream)

	status, body, hdr := runStream(t, srv, chatBody("test-model", "hi", true))

	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "text/event-stream", hdr.Get("Content-Type"))
	assert.Equal(t, "no-cache", hdr.Get("Cache-Control"))
	assert.Equal(t, "no", hdr.Get("X-Accel-Buffering"))

	// Three data frames + [DONE]. We don't compare exact JSON — just
	// confirm the count, ordering, and that finish_reason flowed through.
	frames := splitSSEDataFrames(body)
	require.GreaterOrEqual(t, len(frames), 4, "body=%q", body)
	assert.Contains(t, frames[0], `"role":"assistant"`)
	assert.Contains(t, frames[1], `"content":"hello"`)
	assert.Contains(t, frames[2], `"finish_reason":"stop"`)
	assert.Equal(t, "[DONE]", frames[len(frames)-1])
}

func TestChatStream_PreStreamErrorIsJSON(t *testing.T) {
	// Upstream returns 401 BEFORE any SSE bytes — the gateway must respond
	// with a JSON HTTP error, not an SSE frame, so the client doesn't sit
	// in a stalled SSE parser.
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
	}
	srv := newChatHandlerSrv(t, upstream)

	status, body, hdr := runStream(t, srv, chatBody("test-model", "hi", true))

	assert.Equal(t, http.StatusBadGateway, status)
	assert.Equal(t, "application/json", hdr.Get("Content-Type"))

	var env errorEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	assert.Equal(t, "upstream_auth_failed", env.Error.Code)
}

func TestChatStream_MidStreamErrorEvent(t *testing.T) {
	// Upstream sends one good chunk, then a mid-stream error envelope.
	// Step 3.5 must surface that as `data: {"error":...}\n\n` and stop
	// without emitting [DONE].
	upstream := streamUpstreamSSE(t,
		[]string{
			`data: {"id":"c1","object":"chat.completion.chunk","created":1714000000,"model":"test-model","choices":[{"index":0,"delta":{"content":"first"}}]}` + "\n\n",
			`data: {"error":{"type":"server_error","message":"upstream blew up"}}` + "\n\n",
		},
		false, // no DONE — error is terminal
	)
	srv := newChatHandlerSrv(t, upstream)

	status, body, _ := runStream(t, srv, chatBody("test-model", "hi", true))

	require.Equal(t, http.StatusOK, status, "headers were already 200; status doesn't change mid-stream")
	frames := splitSSEDataFrames(body)
	require.Len(t, frames, 2, "expected 1 data + 1 error event, got body=%q", body)
	assert.Contains(t, frames[0], `"content":"first"`)

	// Last frame is the error envelope. No [DONE].
	var env errorEnvelope
	require.NoError(t, json.Unmarshal([]byte(frames[1]), &env))
	assert.NotEmpty(t, env.Error.Type)
	assert.NotContains(t, body, "[DONE]")
}

func TestChatStream_EOFWithoutDoneIsErrorEvent(t *testing.T) {
	// Upstream closes the stream without [DONE]. The OpenAI provider
	// surfaces this as ErrUpstream (Week 1 carry-over ②); Step 3.5
	// translates that into a final error event.
	upstream := streamUpstreamSSE(t,
		[]string{
			`data: {"id":"c1","object":"chat.completion.chunk","created":1714000000,"model":"test-model","choices":[{"index":0,"delta":{"content":"start"}}]}` + "\n\n",
		},
		false,
	)
	srv := newChatHandlerSrv(t, upstream)

	_, body, _ := runStream(t, srv, chatBody("test-model", "hi", true))

	frames := splitSSEDataFrames(body)
	require.GreaterOrEqual(t, len(frames), 2)
	last := frames[len(frames)-1]
	assert.NotEqual(t, "[DONE]", last)
	assert.Contains(t, last, `"error"`)
}

func TestChatStream_ClientDisconnectStopsCleanly(t *testing.T) {
	// Upstream produces frames slowly. Client cancels. Verify the
	// gateway returns within a small budget — i.e. the consumer loop
	// honored ctx.Done() instead of waiting on the channel forever.
	upstreamReady := make(chan struct{}, 1)
	upstreamRelease := make(chan struct{})
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flushWrite(t, w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
		upstreamReady <- struct{}{}
		// Block until test releases or upstream-side ctx cancels.
		select {
		case <-upstreamRelease:
		case <-r.Context().Done():
		}
	}
	defer close(upstreamRelease)

	srv := newChatHandlerSrv(t, upstream)
	gateway := httptest.NewServer(srv.Handler())
	defer gateway.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gateway.URL+"/v1/chat/completions",
		bytes.NewReader(chatBody("test-model", "hi", true)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Wait for first byte arrival, then yank the client.
	buf := make([]byte, 256)
	_, err = resp.Body.Read(buf)
	require.NoError(t, err)
	<-upstreamReady

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	cancel()

	select {
	case <-done:
		// Gateway handler returned → http response body closed → we got here.
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not stop streaming after client disconnect within 2s")
	}
}

func TestChatStream_NotFoundModel_ReturnsJSON(t *testing.T) {
	srv := newChatHandlerSrv(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream should not be called for unknown model")
	})

	status, body, hdr := runStream(t, srv, chatBody("not-configured", "hi", true))

	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "application/json", hdr.Get("Content-Type"))
	var env errorEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	assert.Equal(t, "model_not_found", env.Error.Code)
}

// splitSSEDataFrames extracts the payload of each `data: ...` line from
// an SSE body, dropping comment lines (`: keepalive`) entirely. Frames
// are returned in arrival order.
func splitSSEDataFrames(body string) []string {
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimRight(frame, "\n")
		if frame == "" {
			continue
		}
		if strings.HasPrefix(frame, ": ") {
			continue
		}
		// Multi-line data is concatenated; for our tests each frame is one line.
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		out = append(out, strings.TrimPrefix(frame, "data: "))
	}
	return out
}

// Sanity: the helper itself parses the format we emit.
func TestSplitSSEDataFrames(t *testing.T) {
	body := "data: a\n\n: keepalive\n\ndata: b\n\ndata: [DONE]\n\n"
	got := splitSSEDataFrames(body)
	assert.Equal(t, []string{"a", "b", "[DONE]"}, got)
}

