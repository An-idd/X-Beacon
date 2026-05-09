package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

func newMeFixture(t *testing.T) (*Server, string) {
	t.Helper()
	const adminKey = "sk-admin-me-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "ak_01H_TEST_ID_1234", Name: "team-frontend", Secret: adminKey,
			Scopes: map[string][]string{"admin": {"webui", "pricing"}}},
	})
	require.NoError(t, err)

	srvReg := registry.NewEmpty()
	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       srvReg,
		Router:         newTestRouter(srvReg),
		Authn:          authn,
		MetricsReg:     prometheus.NewRegistry(),
		MetricsEnabled: false,
	})
	require.NoError(t, err)
	return srv, adminKey
}

func TestAdminMe_RequiresAuth(t *testing.T) {
	srv, _ := newMeFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/me", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminMe_ReturnsPrincipalShape(t *testing.T) {
	srv, key := newMeFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/me", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	assert.Equal(t, "ak_01H_TEST_ID_1234", got["id"])
	assert.Equal(t, "ak_01H_T"[:8], got["id_preview"])
	assert.Equal(t, "team-frontend", got["label"])
	scopes, _ := got["scopes"].([]any)
	require.Len(t, scopes, 2)
	// flattenScopes sorts: admin:pricing < admin:webui
	assert.Equal(t, "admin:pricing", scopes[0])
	assert.Equal(t, "admin:webui", scopes[1])
}
