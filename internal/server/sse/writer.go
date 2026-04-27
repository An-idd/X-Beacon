// Package sse implements a small Server-Sent Events writer used by the
// streaming /v1/chat/completions handler.
//
// Goals (Step 3.5):
//   - serialize SSE frames atomically across data writes and heartbeats
//   - flush each frame so chunks reach the client without buffering
//   - support a heartbeat goroutine that piggy-backs on the same writer
//
// Non-goals: re-implementing chi/cors, gzip, or HTTP/2 push.
package sse

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// ErrNotFlushable is returned by New when the response writer does not
// implement http.Flusher. This happens with naive httptest.ResponseRecorder
// uses; the standard library's httptest.NewRecorder is fine, as is
// http.Server's actual writer.
var ErrNotFlushable = errors.New("sse: response writer does not implement http.Flusher")

// Writer wraps an http.ResponseWriter for atomic SSE frame emission.
// Methods are safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	w  http.ResponseWriter
	f  http.Flusher
}

// New configures w as an SSE response and returns a Writer. Headers are
// written immediately so the first frame can flow without a separate
// commit step.
//
// The X-Accel-Buffering hint disables nginx response buffering for this
// stream — without it, nginx will gather chunks until they fill its
// buffer, defeating SSE.
func New(w http.ResponseWriter) (*Writer, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrNotFlushable
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	return &Writer{w: w, f: f}, nil
}

// WriteData emits a `data: <payload>\n\n` frame and flushes. Payload is
// written verbatim — caller is responsible for JSON marshaling and for
// not embedding raw newlines (SSE spec splits on \n inside data, which
// the OpenAI client tolerates but is fragile).
func (sw *Writer) WriteData(payload []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if _, err := sw.w.Write(dataPrefix); err != nil {
		return err
	}
	if _, err := sw.w.Write(payload); err != nil {
		return err
	}
	if _, err := sw.w.Write(frameEnd); err != nil {
		return err
	}
	sw.f.Flush()
	return nil
}

// WriteComment emits a `: <text>\n\n` line. SSE clients ignore comment
// lines, so this is the standard idiom for keep-alive pings.
func (sw *Writer) WriteComment(text string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if _, err := sw.w.Write(commentPrefix); err != nil {
		return err
	}
	if _, err := sw.w.Write([]byte(text)); err != nil {
		return err
	}
	if _, err := sw.w.Write(frameEnd); err != nil {
		return err
	}
	sw.f.Flush()
	return nil
}

// StartHeartbeat spawns a goroutine that emits a keep-alive comment every
// interval until ctx is canceled or the returned stop func is called.
// Heartbeat writes share the Writer's mutex with normal data writes, so
// frames cannot interleave.
//
// The stop func blocks until the heartbeat goroutine has fully exited;
// callers can defer it to guarantee no writes happen after handler return.
func (sw *Writer) StartHeartbeat(ctx context.Context, interval time.Duration) func() {
	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				// Ignore write errors: a broken pipe at this point means
				// the client is gone, ctx will be canceled imminently, and
				// the next iteration will exit via the Done branch.
				_ = sw.WriteComment("keepalive")
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// dataPrefix / commentPrefix / frameEnd are pre-allocated to avoid
// re-encoding the same constants on every frame.
var (
	dataPrefix    = []byte("data: ")
	commentPrefix = []byte(": ")
	frameEnd      = []byte("\n\n")
)
