package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/storage"
)

// TestCLI_KeygenListRevoke walks the lifecycle end-to-end against a real
// Postgres. Skipped unless XBEACON_TEST_DSN is set.
func TestCLI_KeygenListRevoke(t *testing.T) {
	dsn := os.Getenv("XBEACON_TEST_DSN")
	if dsn == "" {
		t.Skip("set XBEACON_TEST_DSN to run integration tests")
	}

	// Reset the schema so each run starts clean.
	require.NoError(t, storage.MigrateDown(dsn))
	require.NoError(t, storage.MigrateUp(dsn))

	// 1. keygen
	var out bytes.Buffer
	err := runKeygen([]string{"-dsn", dsn, "-name", "integration-test", "-id", "key-int-1"}, &out)
	require.NoError(t, err)
	body := out.String()
	assert.Contains(t, body, "key-int-1")
	assert.Contains(t, body, "secret: sk-")
	out.Reset()

	// 2. keylist (default: active only) → should include the new key
	err = runKeylist([]string{"-dsn", dsn}, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "key-int-1")
	out.Reset()

	// 2b. keylist --json → parses
	err = runKeylist([]string{"-dsn", dsn, "-json"}, &out)
	require.NoError(t, err)
	jsonOut := out.String()
	assert.True(t, strings.Contains(jsonOut, `"id": "key-int-1"`),
		"expected JSON entry for key-int-1, got %s", jsonOut)
	out.Reset()

	// 3. keyrevoke
	err = runKeyrevoke([]string{"-dsn", dsn, "-id", "key-int-1"}, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Revoked")
	out.Reset()

	// 4. keyrevoke again is idempotent (already-revoked path)
	err = runKeyrevoke([]string{"-dsn", dsn, "-id", "key-int-1"}, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "already revoked")
	out.Reset()

	// 5. keylist default hides revoked keys
	err = runKeylist([]string{"-dsn", dsn}, &out)
	require.NoError(t, err)
	assert.NotContains(t, out.String(), "key-int-1")
	out.Reset()

	// 6. keylist --all surfaces them
	err = runKeylist([]string{"-dsn", dsn, "-all"}, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "key-int-1")
	out.Reset()

	// 7. keyrevoke nonexistent ID → error
	err = runKeyrevoke([]string{"-dsn", dsn, "-id", "no-such-key"}, &out)
	require.Error(t, err)
}

func TestCLI_Migrate_VersionRoundTrip(t *testing.T) {
	dsn := os.Getenv("XBEACON_TEST_DSN")
	if dsn == "" {
		t.Skip("set XBEACON_TEST_DSN to run integration tests")
	}

	require.NoError(t, storage.MigrateDown(dsn))

	var out bytes.Buffer
	require.NoError(t, runMigrate([]string{"-dsn", dsn, "version"}, &out))
	assert.Contains(t, out.String(), "version=0")
	out.Reset()

	require.NoError(t, runMigrate([]string{"-dsn", dsn, "up"}, &out))
	assert.Contains(t, out.String(), "head")
	out.Reset()

	require.NoError(t, runMigrate([]string{"-dsn", dsn, "version"}, &out))
	assert.Contains(t, out.String(), "version=1")
	out.Reset()

	require.NoError(t, runMigrate([]string{"-dsn", dsn, "down"}, &out))
	out.Reset()
}
