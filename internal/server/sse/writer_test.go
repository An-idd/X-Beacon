package sse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nonFlusher is an http.ResponseWriter without Flusher, used to verify
// New rejects writers that can't deliver chunks.
type nonFlusher struct {
	header http.Header
}

func (f *nonFlusher) Header() http.Header       { return f.header }
func (f *nonFlusher) Write(b []byte) (int, error) { return len(b), nil }
func (f *nonFlusher) WriteHeader(int)            {}

func TestNew_RejectsNonFlusher(t *testing.T) {
	_, err := New(&nonFlusher{header: http.Header{}})
	assert.ErrorIs(t, err, ErrNotFlushable)
}

func TestNew_SetsHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := New(rec)
	require.NoError(t, err)

	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", rec.Header().Get("Connection"))
	assert.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))
}

func TestWriteData_FormatsAndFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := New(rec)
	require.NoError(t, err)

	require.NoError(t, sw.WriteData([]byte(`{"id":"1"}`)))

	assert.Equal(t, "data: {\"id\":\"1\"}\n\n", rec.Body.String())
	assert.True(t, rec.Flushed)
}

func TestWriteComment_FormatsAndFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := New(rec)
	require.NoError(t, err)

	require.NoError(t, sw.WriteComment("keepalive"))

	assert.Equal(t, ": keepalive\n\n", rec.Body.String())
	assert.True(t, rec.Flushed)
}

func TestWriteData_MultipleFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := New(rec)
	require.NoError(t, err)

	require.NoError(t, sw.WriteData([]byte("a")))
	require.NoError(t, sw.WriteData([]byte("b")))
	require.NoError(t, sw.WriteData([]byte("[DONE]")))

	assert.Equal(t, "data: a\n\ndata: b\n\ndata: [DONE]\n\n", rec.Body.String())
}

func TestWriter_ConcurrentWritesAreFramed(t *testing.T) {
	// Stress concurrent writes — the mutex must prevent torn frames.
	// Each frame is 3 lines: "data: <payload>\n\n". Concurrent writes
	// must yield a body whose frames are intact (each ends with \n\n
	// and has no embedded \n inside data: line).
	rec := httptest.NewRecorder()
	sw, err := New(rec)
	require.NoError(t, err)

	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				_ = sw.WriteData([]byte("chunk"))
			}
		}()
	}
	wg.Wait()

	body := rec.Body.String()
	frames := strings.Split(body, "\n\n")
	// Last split is empty (string ends with \n\n).
	frames = frames[:len(frames)-1]
	assert.Equal(t, writers*perWriter, len(frames))
	for _, f := range frames {
		assert.Equal(t, "data: chunk", f, "torn frame detected")
	}
}

func TestStartHeartbeat_FiresAtInterval(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := New(rec)
	require.NoError(t, err)

	stop := sw.StartHeartbeat(context.Background(), 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	stop()

	body := rec.Body.String()
	count := strings.Count(body, ": keepalive\n\n")
	assert.GreaterOrEqual(t, count, 2, "heartbeat did not fire enough times: body=%q", body)
}

func TestStartHeartbeat_StopsOnContextCancel(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := New(rec)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	stop := sw.StartHeartbeat(ctx, 5*time.Millisecond)

	time.Sleep(15 * time.Millisecond)
	cancel()
	stop() // must return promptly even though we already canceled

	beforeLen := rec.Body.Len()
	time.Sleep(15 * time.Millisecond)
	assert.Equal(t, beforeLen, rec.Body.Len(), "heartbeat continued after cancel")
}

func TestStartHeartbeat_StopFuncIsBlocking(t *testing.T) {
	// stop must not return until the goroutine has exited — otherwise the
	// caller can't safely return from the handler without racing with a
	// pending WriteComment.
	rec := httptest.NewRecorder()
	sw, err := New(rec)
	require.NoError(t, err)

	stop := sw.StartHeartbeat(context.Background(), 1*time.Millisecond)
	stop()

	// After stop returns, no further writes should appear.
	frozen := rec.Body.Len()
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, frozen, rec.Body.Len())
}
