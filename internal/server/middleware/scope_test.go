package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
)

func TestRequireScope_Allows(t *testing.T) {
	called := false
	h := RequireScope("admin", "pricing", zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "p1", Scopes: map[string][]string{"admin": {"pricing"}},
	})
	req := httptest.NewRequest("GET", "/admin/pricing", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireScope_AllowsWildcard(t *testing.T) {
	h := RequireScope("admin", "pricing", zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "p1", Scopes: map[string][]string{"admin": {"*"}},
	})
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireScope_BlocksMissingPrincipal(t *testing.T) {
	called := false
	h := RequireScope("admin", "pricing", zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.False(t, called, "handler must not run without a principal")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "insufficient_scope")
}

func TestRequireScope_BlocksWrongCategory(t *testing.T) {
	h := RequireScope("admin", "pricing", zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "p1", Scopes: map[string][]string{"rate": {"pricing"}},
	})
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestRequireScope_BlocksWrongValue(t *testing.T) {
	h := RequireScope("admin", "pricing", zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{
		ID: "p1", Scopes: map[string][]string{"admin": {"users"}},
	})
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
