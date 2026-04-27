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

func TestRecovery_PanicReturns500(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	logger := zap.New(core)

	h := Recovery(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/explode", nil)
	rec := httptest.NewRecorder()

	assert.NotPanics(t, func() { h.ServeHTTP(rec, req) })
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "internal server error")
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "panic recovered", entry.Message)
	assert.Equal(t, zap.ErrorLevel, entry.Level)
	// Verify path, method, panic value, stack are all attached.
	fields := entry.ContextMap()
	assert.Equal(t, "GET", fields["method"])
	assert.Equal(t, "/explode", fields["path"])
	assert.Equal(t, "boom", fields["panic"])
	assert.NotEmpty(t, fields["stack"])
}

func TestRecovery_NoOpWhenNoPanic(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	logger := zap.New(core)

	h := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, 0, logs.Len(), "no recovery log when no panic")
}

func TestRecovery_RepanicsAbortHandler(t *testing.T) {
	logger := zap.NewNop()
	h := Recovery(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	// http.ErrAbortHandler is the documented "abort silently" signal; net/http
	// itself recovers from it. Recovery must re-panic so the runtime catches it.
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() { h.ServeHTTP(rec, req) })
}
