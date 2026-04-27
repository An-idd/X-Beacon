package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// withRequestIDChain wraps handler in Logging then RequestID so tests get
// a populated req_id field without rolling the chain by hand each time.
func withRequestIDChain(handler http.Handler, logger *zap.Logger, opts LoggingOptions) http.Handler {
	return RequestID()(Logging(logger, opts)(handler))
}

func TestLogging_LogsBasicFields(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})
	chain := withRequestIDChain(h, logger, LoggingOptions{})

	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "http request", entry.Message)
	assert.Equal(t, zap.InfoLevel, entry.Level)
	fields := entry.ContextMap()
	assert.Equal(t, "GET", fields["method"])
	assert.Equal(t, "/foo", fields["path"])
	assert.Equal(t, int64(200), fields["status"])
	assert.Equal(t, int64(5), fields["bytes"])
	assert.NotEmpty(t, fields["req_id"])
}

func TestLogging_4xxIsWarn(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	chain := withRequestIDChain(h, logger, LoggingOptions{})

	req := httptest.NewRequest(http.MethodPost, "/bad", nil)
	chain.ServeHTTP(httptest.NewRecorder(), req)

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, zap.WarnLevel, logs.All()[0].Level)
}

func TestLogging_5xxIsError(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	logger := zap.New(core)

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	chain := withRequestIDChain(h, logger, LoggingOptions{})

	req := httptest.NewRequest(http.MethodGet, "/oops", nil)
	chain.ServeHTTP(httptest.NewRecorder(), req)

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, zap.ErrorLevel, logs.All()[0].Level)
}

func TestLogging_SkipPathsSuppressed(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	chain := withRequestIDChain(h, logger, LoggingOptions{SkipPaths: []string{"/metrics"}})

	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	assert.Equal(t, 1, logs.Len(), "only /v1/models should produce a log")
	assert.Equal(t, "/v1/models", logs.All()[0].ContextMap()["path"])
}

func TestLogging_DefaultStatusIsOK(t *testing.T) {
	// Handler writes body without explicit WriteHeader: Go writes implicit 200,
	// loggingResponseWriter must capture that.
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	chain := withRequestIDChain(h, logger, LoggingOptions{})

	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, int64(200), logs.All()[0].ContextMap()["status"])
}

func TestLoggingResponseWriter_FlushForwards(t *testing.T) {
	// Use httptest.NewRecorder which DOES implement Flusher (since Go 1.6).
	rec := httptest.NewRecorder()
	lrw := newLoggingResponseWriter(rec)

	// Flush before any write — must not panic, must not write headers.
	assert.NotPanics(t, lrw.Flush)
}
