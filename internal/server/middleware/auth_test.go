package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
)

// fakeAuthn lets tests script Authenticate's outcome without dragging in
// the static implementation (which would couple this test to its hashing).
type fakeAuthn struct {
	want      string
	principal *auth.Principal
	failWith  error
}

func (f *fakeAuthn) Authenticate(_ context.Context, key string) (*auth.Principal, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	if key == f.want {
		return f.principal, nil
	}
	return nil, auth.ErrInvalidCredentials
}

func TestAuth_ValidToken_PrincipalInContext(t *testing.T) {
	authn := &fakeAuthn{want: "sk-good", principal: &auth.Principal{ID: "k1", Name: "test"}}
	var captured *auth.Principal
	h := Auth(authn, zap.NewNop())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = auth.PrincipalFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.NotNil(t, captured)
	assert.Equal(t, "k1", captured.ID)
}

func TestAuth_NoHeader_Returns401MissingCreds(t *testing.T) {
	h := Auth(&fakeAuthn{}, zap.NewNop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errBody, _ := body["error"].(map[string]any)
	assert.Equal(t, "missing_credentials", errBody["code"])
	assert.NotEmpty(t, errBody["message"])
}

func TestAuth_BadScheme_Returns401(t *testing.T) {
	h := Auth(&fakeAuthn{}, zap.NewNop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_BearerCaseInsensitive(t *testing.T) {
	authn := &fakeAuthn{want: "tok", principal: &auth.Principal{ID: "k1"}}
	h := Auth(authn, zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
		req.Header.Set("Authorization", scheme+" tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "scheme %q", scheme)
	}
}

func TestAuth_EmptyToken_Returns401(t *testing.T) {
	h := Auth(&fakeAuthn{}, zap.NewNop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_InvalidToken_Returns401InvalidCreds(t *testing.T) {
	authn := &fakeAuthn{want: "sk-good", principal: &auth.Principal{ID: "k1"}}
	h := Auth(authn, zap.NewNop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer sk-bad")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "invalid_credentials", body["error"].(map[string]any)["code"])
}

func TestAuth_BackendError_Returns500(t *testing.T) {
	authn := &fakeAuthn{failWith: assertedBackendErr}
	h := Auth(authn, zap.NewNop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "internal_error", body["error"].(map[string]any)["code"])
}

func TestAuth_DoesNotEchoToken(t *testing.T) {
	authn := &fakeAuthn{want: "sk-good", principal: &auth.Principal{ID: "k1"}}
	h := Auth(authn, zap.NewNop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer sk-secret-leak-canary")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.NotContains(t, rec.Body.String(), "sk-secret-leak-canary",
		"auth response leaked the supplied bearer token")
}

// assertedBackendErr is a sentinel for "unexpected backend failure" used
// by the 500 path test. Unrelated to auth's own sentinels.
var assertedBackendErr = errBackend("simulated backend outage")

type errBackend string

func (e errBackend) Error() string { return string(e) }
