package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newCORSHandler(t *testing.T, origins []string) http.Handler {
	t.Helper()
	return CORS(origins)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
}

func TestCORS_AllowedOriginEchoed(t *testing.T) {
	h := newCORSHandler(t, []string{"https://admin.example.com"})

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "https://admin.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, rec.Header().Get("Vary"), "Origin")
}

func TestCORS_DisallowedOriginNoHeaders(t *testing.T) {
	h := newCORSHandler(t, []string{"https://admin.example.com"})

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Request still succeeds (server-side); browser SOP does the rejection.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))
	// Vary still set so caches don't pollute.
	assert.Contains(t, rec.Header().Get("Vary"), "Origin")
}

func TestCORS_PreflightWithoutAuthSucceeds(t *testing.T) {
	h := newCORSHandler(t, []string{"https://admin.example.com"})

	req := httptest.NewRequest(http.MethodOptions, "/admin/keys", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "https://admin.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "POST")
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	assert.Equal(t, "600", rec.Header().Get("Access-Control-Max-Age"))
}

func TestCORS_EmptyAllowlistNoOp(t *testing.T) {
	h := newCORSHandler(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
		"empty allowlist must not emit any CORS headers")
}

func TestCORS_NoOriginHeaderPassesThrough(t *testing.T) {
	// Server-to-server requests (no browser) have no Origin header.
	// Middleware should be a transparent passthrough.
	h := newCORSHandler(t, []string{"https://admin.example.com"})

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, rec.Header().Get("Vary"))
}

func TestCORS_PreflightDisallowedOriginNoEchoButStill204(t *testing.T) {
	// Even disallowed origins get a 204 on OPTIONS — without the echo,
	// the browser's SOP refuses the followup request. Returning 4xx on
	// preflight would leak which origins are configured.
	h := newCORSHandler(t, []string{"https://admin.example.com"})

	req := httptest.NewRequest(http.MethodOptions, "/admin/keys", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}
