package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPool_RequiresDSN(t *testing.T) {
	_, err := NewPool(context.Background(), Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DSN is required")
}

func TestNewPool_RejectsMalformedDSN(t *testing.T) {
	_, err := NewPool(context.Background(), Config{DSN: "not a dsn"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse DSN")
}

func TestNewPool_LazyConstruction(t *testing.T) {
	// pgxpool.NewWithConfig does not connect; constructing a pool against
	// an unreachable host succeeds. Network failure surfaces only on Ping.
	pool, err := NewPool(context.Background(), Config{
		DSN:             "postgres://nobody:nopass@127.0.0.1:1/none?sslmode=disable",
		MaxConns:        4,
		MinConns:        1,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	})
	require.NoError(t, err)
	defer pool.Close()
	assert.NotNil(t, pool.Pool)
}

func TestPool_NilSafe(t *testing.T) {
	var p *Pool
	// Ping on nil should error, not panic.
	err := p.Ping(context.Background())
	require.Error(t, err)
	assert.NotPanics(t, func() { p.Close() })
}

// TestPool_PingIntegration verifies a real connection. Skipped unless
// XBEACON_TEST_DSN is set (CI / local run with `docker compose up postgres`).
//
// Sample command:
//
//	XBEACON_TEST_DSN=postgres://xbeacon:xbeacon@localhost:5432/xbeacon?sslmode=disable \
//	  go test ./internal/storage/...
func TestPool_PingIntegration(t *testing.T) {
	dsn := os.Getenv("XBEACON_TEST_DSN")
	if dsn == "" {
		t.Skip("set XBEACON_TEST_DSN to run integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, Config{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	defer pool.Close()

	require.NoError(t, pool.Ping(ctx))
}
