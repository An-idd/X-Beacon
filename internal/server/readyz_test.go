package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func okChecker(name string) ReadinessChecker {
	return ReadinessChecker{
		Name:  name,
		Check: func(context.Context) error { return nil },
	}
}

func failChecker(name, errMsg string) ReadinessChecker {
	return ReadinessChecker{
		Name:  name,
		Check: func(context.Context) error { return errors.New(errMsg) },
	}
}

func runReadyz(t *testing.T, checkers []ReadinessChecker) (*httptest.ResponseRecorder, readyzResponse) {
	t.Helper()
	srv := newTestServer(t, func(d *Deps) { d.ReadinessCheckers = checkers })
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	var body readyzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec, body
}

func TestReadyz_AllPass(t *testing.T) {
	rec, body := runReadyz(t, []ReadinessChecker{okChecker("postgres"), okChecker("redis")})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, body.Ready)
	assert.True(t, body.Checks["postgres"].OK)
	assert.True(t, body.Checks["redis"].OK)
}

func TestReadyz_OneFails_Returns503(t *testing.T) {
	rec, body := runReadyz(t, []ReadinessChecker{
		okChecker("postgres"),
		failChecker("redis", "connection refused"),
	})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.False(t, body.Ready)
	assert.True(t, body.Checks["postgres"].OK)
	assert.False(t, body.Checks["redis"].OK)
	assert.Equal(t, "connection refused", body.Checks["redis"].Error)
}

func TestReadyz_AllFail(t *testing.T) {
	rec, body := runReadyz(t, []ReadinessChecker{
		failChecker("postgres", "p down"),
		failChecker("redis", "r down"),
	})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.False(t, body.Ready)
	assert.Equal(t, "p down", body.Checks["postgres"].Error)
	assert.Equal(t, "r down", body.Checks["redis"].Error)
}

func TestReadyz_NoCheckers_ReturnsOK(t *testing.T) {
	rec, body := runReadyz(t, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, body.Ready)
	assert.Empty(t, body.Checks)
}

func TestReadyz_TimeoutSurfacesAsCheckerError(t *testing.T) {
	// A checker that hangs longer than readinessTimeout: the deadline
	// kicks in, ctx.Err propagates, and the response 503s with
	// "context deadline exceeded".
	slow := ReadinessChecker{
		Name: "slow",
		Check: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		},
	}
	rec, body := runReadyz(t, []ReadinessChecker{slow})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.False(t, body.Ready)
	assert.Contains(t, body.Checks["slow"].Error, "context deadline exceeded")
}

func TestReadyz_NotInsideV1Auth(t *testing.T) {
	// /readyz must be reachable WITHOUT a bearer token, otherwise k8s
	// probes can't query it.
	srv := newTestServer(t, func(d *Deps) {
		d.ReadinessCheckers = []ReadinessChecker{okChecker("x")}
	})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}
