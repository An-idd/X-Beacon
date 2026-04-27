package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freePort returns a port that is not currently bound. We pick fresh
// ports for lifecycle tests instead of hardcoding so parallel test
// runs (or a stuck previous run) can't collide.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func TestParseArgs_Defaults(t *testing.T) {
	configPath, showVersion, err := parseArgs(nil)
	require.NoError(t, err)
	assert.Equal(t, "configs/config.yaml", configPath)
	assert.False(t, showVersion)
}

func TestParseArgs_CustomConfig(t *testing.T) {
	configPath, _, err := parseArgs([]string{"-config", "/tmp/x.yaml"})
	require.NoError(t, err)
	assert.Equal(t, "/tmp/x.yaml", configPath)
}

func TestParseArgs_VersionFlag(t *testing.T) {
	_, showVersion, err := parseArgs([]string{"-version"})
	require.NoError(t, err)
	assert.True(t, showVersion)
}

func TestParseArgs_UnknownFlagErrors(t *testing.T) {
	_, _, err := parseArgs([]string{"-this-flag-does-not-exist"})
	require.Error(t, err)
}

// captureStdout redirects os.Stdout for the duration of fn so tests can
// assert on text the runner prints (e.g. --version output).
func captureStdout(t *testing.T, fn func(out *os.File)) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)

	fn(w)
	require.NoError(t, w.Close())

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

func TestRunWithCtx_Version(t *testing.T) {
	out := captureStdout(t, func(w *os.File) {
		err := runWithCtx(context.Background(), []string{"-version"}, w)
		require.NoError(t, err)
	})
	assert.Contains(t, out, "x-beacon")
}

func TestRunWithCtx_BadConfig(t *testing.T) {
	err := runWithCtx(context.Background(), []string{"-config", "/nonexistent/x.yaml"}, os.Stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load config")
}

func TestRunWithCtx_LifecycleCleanShutdown(t *testing.T) {
	// Build a minimal valid config: bind to a free port, no providers.yaml,
	// no auth.yaml — the gateway should still come up (dev-mode behavior),
	// then exit cleanly when ctx is canceled.
	addr := freePort(t)
	cfg := strings.NewReplacer(
		"{{addr}}", addr,
	).Replace(`
server:
  addr: "{{addr}}"
  read_timeout: 5s
  write_timeout: 30s
  shutdown_timeout: 5s
log:
  level: error
  format: json
observability:
  metrics:
    enabled: false
    path: /metrics
  tracing:
    enabled: false
providers_file: /nonexistent/providers.yaml
auth_file: /nonexistent/auth.yaml
`)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithCtx(ctx, []string{"-config", cfgPath}, os.Stdout)
	}()

	// Give the listener a beat to bind, then signal shutdown.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("runWithCtx did not return within 5s after ctx cancel")
	}
}
