// Package observability provides constructors for the gateway's three pillars:
// structured logging (Zap), metrics (Prometheus), and distributed tracing
// (OpenTelemetry). Callers must invoke shutdown functions returned here at
// process exit to flush buffered data.
package observability

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type LogConfig struct {
	Level  string // debug | info | warn | error; empty means info
	Format string // json | console; empty means json
}

func NewLogger(cfg LogConfig) (*zap.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	zapCfg := zap.NewProductionConfig()
	zapCfg.Level = zap.NewAtomicLevelAt(level)
	zapCfg.EncoderConfig.TimeKey = "ts"
	zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	switch strings.ToLower(cfg.Format) {
	case "", "json":
		zapCfg.Encoding = "json"
	case "console":
		zapCfg.Encoding = "console"
		zapCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	default:
		return nil, fmt.Errorf("invalid log format %q", cfg.Format)
	}

	logger, err := zapCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}
	return logger, nil
}

func parseLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return zapcore.InfoLevel, nil
	case "debug":
		return zapcore.DebugLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return 0, fmt.Errorf("invalid log level %q", s)
	}
}
