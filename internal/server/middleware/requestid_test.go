package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	var captured string
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.NotEmpty(t, captured)
	// Verify it's a UUIDv4 — anything else is a regression.
	parsed, err := uuid.Parse(captured)
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(4), parsed.Version())

	assert.Equal(t, captured, rec.Header().Get(HeaderRequestID))
}

func TestRequestID_PassesThroughInbound(t *testing.T) {
	const inbound = "req-from-upstream-7e3"
	var captured string
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, inbound)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, inbound, captured)
	assert.Equal(t, inbound, rec.Header().Get(HeaderRequestID))
}

func TestRequestID_RejectsOverlongInbound(t *testing.T) {
	long := strings.Repeat("a", 200)
	var captured string
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, long)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.NotEqual(t, long, captured, "overlong inbound ID must be replaced")
	_, err := uuid.Parse(captured)
	require.NoError(t, err)
}

func TestRequestIDFrom_NoValueReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", RequestIDFrom(context.Background()))
}
