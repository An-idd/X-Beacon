package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/storage"
)

// adminFixture sets up a Server with admin/pricing routes wired against
// a real Postgres pool (gated by XBEACON_TEST_DSN). Returns the server,
// the bearer key with admin:pricing scope, and a no-scope key.
type adminFixture struct {
	srv         *Server
	adminKey    string
	noScopeKey  string
	pool        *storage.Pool
	cache       *billing.PricingCache
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	dsn := os.Getenv("XBEACON_TEST_DSN")
	if dsn == "" {
		t.Skip("set XBEACON_TEST_DSN to run admin/pricing integration tests")
	}
	require.NoError(t, storage.MigrateDown(dsn))
	require.NoError(t, storage.MigrateUp(dsn))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cache := billing.NewPricingCache(pool, zap.NewNop())
	require.NoError(t, cache.Reload(context.Background()))

	const adminKey = "sk-admin-test"
	const noScopeKey = "sk-noscope-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "admin", Name: "Admin", Secret: adminKey, Scopes: map[string][]string{"admin": {"pricing"}}},
		{ID: "user", Name: "Regular", Secret: noScopeKey},
	})
	require.NoError(t, err)

	reg := registry.NewEmpty()
	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         newTestRouter(reg),
		Authn:          authn,
		Pricing:        cache,
		MetricsReg:     prometheus.NewRegistry(),
		MetricsEnabled: true,
		MetricsPath:    "/metrics",
	})
	require.NoError(t, err)

	return &adminFixture{srv: srv, adminKey: adminKey, noScopeKey: noScopeKey, pool: pool, cache: cache}
}

func (f *adminFixture) do(t *testing.T, method, path string, body []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAdminPricing_ListRequiresScope(t *testing.T) {
	f := newAdminFixture(t)
	// No bearer → 401
	rec := f.do(t, "GET", "/admin/pricing", nil, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Wrong scope → 403
	rec = f.do(t, "GET", "/admin/pricing", nil, f.noScopeKey)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "insufficient_scope")
}

func TestAdminPricing_ListReturnsSeededRows(t *testing.T) {
	f := newAdminFixture(t)
	rec := f.do(t, "GET", "/admin/pricing", nil, f.adminKey)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Object string       `json:"object"`
		Data   []pricingDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "list", resp.Object)
	assert.GreaterOrEqual(t, len(resp.Data), 10) // migration seeds 10 rows

	// Verify alphabetical order.
	for i := 1; i < len(resp.Data); i++ {
		assert.LessOrEqual(t, resp.Data[i-1].Model, resp.Data[i].Model)
	}
}

func TestAdminPricing_GetExistingAndMissing(t *testing.T) {
	f := newAdminFixture(t)

	rec := f.do(t, "GET", "/admin/pricing/gpt-4o", nil, f.adminKey)
	require.Equal(t, http.StatusOK, rec.Code)
	var dto pricingDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dto))
	assert.Equal(t, "gpt-4o", dto.Model)
	assert.Equal(t, int64(5000), dto.InputPer1kMicro)

	rec = f.do(t, "GET", "/admin/pricing/nonexistent", nil, f.adminKey)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "model_not_priced")
}

func TestAdminPricing_UpsertWithFloat(t *testing.T) {
	f := newAdminFixture(t)
	body, _ := json.Marshal(map[string]any{
		"input_per_1k":  0.001, // 1000 micro
		"output_per_1k": 0.003, // 3000 micro
	})
	rec := f.do(t, "PUT", "/admin/pricing/test-model", body, f.adminKey)
	require.Equal(t, http.StatusOK, rec.Code)

	var dto pricingDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dto))
	assert.Equal(t, "test-model", dto.Model)
	assert.Equal(t, int64(1000), dto.InputPer1kMicro)
	assert.Equal(t, int64(3000), dto.OutputPer1kMicro)

	// Cache must be in-sync (same instance).
	r, ok := f.cache.Lookup("test-model")
	require.True(t, ok)
	assert.Equal(t, int64(1000), r.InputPer1kMicro)
}

func TestAdminPricing_UpsertWithMicroIntegers(t *testing.T) {
	f := newAdminFixture(t)
	body, _ := json.Marshal(map[string]any{
		"input_per_1k_micro":  int64(7777),
		"output_per_1k_micro": int64(8888),
		"currency":            "USD",
	})
	rec := f.do(t, "PUT", "/admin/pricing/exact-model", body, f.adminKey)
	require.Equal(t, http.StatusOK, rec.Code)

	r, _ := f.cache.Lookup("exact-model")
	assert.Equal(t, int64(7777), r.InputPer1kMicro)
	assert.Equal(t, int64(8888), r.OutputPer1kMicro)
}

func TestAdminPricing_UpsertScopeRejected(t *testing.T) {
	f := newAdminFixture(t)
	body, _ := json.Marshal(map[string]any{"input_per_1k": 0.001})
	rec := f.do(t, "PUT", "/admin/pricing/test-model", body, f.noScopeKey)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAdminPricing_DeleteHappyPathAndIdempotent(t *testing.T) {
	f := newAdminFixture(t)
	rec := f.do(t, "DELETE", "/admin/pricing/gpt-4o", nil, f.adminKey)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	_, ok := f.cache.Lookup("gpt-4o")
	assert.False(t, ok)

	// Second delete → 404
	rec = f.do(t, "DELETE", "/admin/pricing/gpt-4o", nil, f.adminKey)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminPricing_UpsertMalformedJSON(t *testing.T) {
	f := newAdminFixture(t)
	rec := f.do(t, "PUT", "/admin/pricing/test", []byte("not json"), f.adminKey)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
