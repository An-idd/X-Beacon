package storage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToMigrateURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"postgres://u:p@h:5432/db?sslmode=disable", "pgx5://u:p@h:5432/db?sslmode=disable"},
		{"postgresql://u:p@h:5432/db", "pgx5://u:p@h:5432/db"},
		{"pgx5://u:p@h:5432/db", "pgx5://u:p@h:5432/db"},
		{"unknown-scheme://x", "unknown-scheme://x"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, toMigrateURL(c.in), "in=%q", c.in)
	}
}

func TestMigrationsFS_Embeds(t *testing.T) {
	// Sanity: the embed directive picks up at least the first migration.
	entries, err := migrationsFS.ReadDir(migrationsDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	var sawUp, sawDown bool
	for _, e := range entries {
		if strings.Contains(e.Name(), "create_api_keys.up.sql") {
			sawUp = true
		}
		if strings.Contains(e.Name(), "create_api_keys.down.sql") {
			sawDown = true
		}
	}
	assert.True(t, sawUp, "missing 000001_create_api_keys.up.sql in embed")
	assert.True(t, sawDown, "missing 000001_create_api_keys.down.sql in embed")
}

func TestMigrateUp_BadDSN(t *testing.T) {
	err := MigrateUp("not a dsn")
	require.Error(t, err)
}

// Integration: round-trip up → version → down against a real Postgres.
// Skipped unless XBEACON_TEST_DSN points at a writable database.
//
// Run locally with:
//
//	docker compose up -d postgres
//	XBEACON_TEST_DSN=postgres://xbeacon:xbeacon@localhost:5432/xbeacon?sslmode=disable \
//	  go test ./internal/storage/...
func TestMigrate_RoundTripIntegration(t *testing.T) {
	dsn := os.Getenv("XBEACON_TEST_DSN")
	if dsn == "" {
		t.Skip("set XBEACON_TEST_DSN to run integration tests")
	}

	// Reset before to keep the test idempotent (down is a no-op on empty schema).
	_ = MigrateDown(dsn)

	require.NoError(t, MigrateUp(dsn))

	v, dirty, err := MigrateVersion(dsn)
	require.NoError(t, err)
	assert.Equal(t, uint(1), v)
	assert.False(t, dirty)

	// Verify the table exists by Pinging then querying information_schema.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, Config{DSN: dsn})
	require.NoError(t, err)
	defer pool.Close()

	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='api_keys')`,
	).Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists)

	require.NoError(t, MigrateDown(dsn))

	v2, _, err := MigrateVersion(dsn)
	require.NoError(t, err)
	assert.Equal(t, uint(0), v2, "down should leave schema empty")
}
