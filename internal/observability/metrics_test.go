package observability

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMetricsRegistry(t *testing.T) {
	reg := NewMetricsRegistry()
	require.NotNil(t, reg)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	// Go + process collectors yield many metric families; assert a healthy lower bound.
	assert.Greater(t, len(mfs), 5)
}
