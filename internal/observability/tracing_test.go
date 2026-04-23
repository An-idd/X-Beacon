package observability

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTracerProvider_Disabled(t *testing.T) {
	ctx := context.Background()
	tp, shutdown, err := NewTracerProvider(ctx, TracingConfig{
		Enabled:     false,
		ServiceName: "x-beacon-test",
		SampleRatio: 1.0,
	})
	require.NoError(t, err)
	require.NotNil(t, tp)
	require.NotNil(t, shutdown)

	tracer := tp.Tracer("test")
	_, span := tracer.Start(ctx, "noop")
	span.End()

	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	assert.NoError(t, shutdown(sctx))
}

// TestNewTracerProvider_Enabled only verifies the constructor contract:
// OTel's otlp HTTP client creation is sync but export happens async, so an
// unreachable endpoint must not fail construction. Shutdown may return a
// network error; we tolerate that.
func TestNewTracerProvider_Enabled(t *testing.T) {
	ctx := context.Background()
	tp, shutdown, err := NewTracerProvider(ctx, TracingConfig{
		Enabled:     true,
		Endpoint:    "127.0.0.1:1",
		ServiceName: "x-beacon-test",
		SampleRatio: 1.0,
	})
	require.NoError(t, err)
	require.NotNil(t, tp)

	sctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	_ = shutdown(sctx)
}
