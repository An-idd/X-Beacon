package observability

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestNewLogger_Defaults(t *testing.T) {
	logger, err := NewLogger(LogConfig{})
	require.NoError(t, err)
	require.NotNil(t, logger)
	assert.True(t, logger.Core().Enabled(zapcore.InfoLevel))
	assert.False(t, logger.Core().Enabled(zapcore.DebugLevel))
}

func TestNewLogger_Debug(t *testing.T) {
	logger, err := NewLogger(LogConfig{Level: "debug"})
	require.NoError(t, err)
	assert.True(t, logger.Core().Enabled(zapcore.DebugLevel))
}

func TestNewLogger_Console(t *testing.T) {
	_, err := NewLogger(LogConfig{Level: "info", Format: "console"})
	require.NoError(t, err)
}

func TestNewLogger_InvalidLevel(t *testing.T) {
	_, err := NewLogger(LogConfig{Level: "bogus"})
	require.Error(t, err)
}

func TestNewLogger_InvalidFormat(t *testing.T) {
	_, err := NewLogger(LogConfig{Level: "info", Format: "xml"})
	require.Error(t, err)
}
