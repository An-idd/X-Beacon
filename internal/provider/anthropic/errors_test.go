package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

func TestMapToSentinel_ByErrorType(t *testing.T) {
	// When Anthropic provides error.type, it dominates over HTTP status.
	tests := []struct {
		errType string
		status  int // arbitrary; error.type should win
		want    error
	}{
		{"authentication_error", 500, provider.ErrAuth},
		{"permission_error", 400, provider.ErrAuth},
		{"rate_limit_error", 400, provider.ErrRateLimited},
		{"overloaded_error", 200, provider.ErrUnavailable},
		{"invalid_request_error", 500, provider.ErrInvalidRequest},
		{"not_found_error", 500, provider.ErrInvalidRequest},
		{"request_too_large", 200, provider.ErrContextLength},
		{"api_error", 200, provider.ErrUpstream},
	}
	for _, tc := range tests {
		t.Run(tc.errType, func(t *testing.T) {
			got := mapToSentinel(tc.status, tc.errType)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMapToSentinel_ByStatusFallback(t *testing.T) {
	// When error.type is absent/unknown, HTTP status drives the decision.
	tests := []struct {
		status int
		want   error
	}{
		{401, provider.ErrAuth},
		{403, provider.ErrAuth},
		{429, provider.ErrRateLimited},
		{400, provider.ErrInvalidRequest},
		{404, provider.ErrInvalidRequest},
		{422, provider.ErrInvalidRequest},
		{408, provider.ErrTimeout},
		{503, provider.ErrUnavailable},
		{529, provider.ErrUnavailable}, // Anthropic's Overloaded status
		{500, provider.ErrUpstream},
		{502, provider.ErrUpstream},
		{504, provider.ErrUpstream},
		{418, provider.ErrUpstream}, // unknown 4xx
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			got := mapToSentinel(tc.status, "")
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMapHTTPError_JSONBody(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	header := http.Header{"Retry-After": []string{"30"}}

	err := mapHTTPError("anthropic-primary", 429, body, header)
	require.Error(t, err)

	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.Equal(t, "anthropic-primary", ue.Provider)
	assert.Equal(t, 429, ue.StatusCode)
	assert.Equal(t, 30*time.Second, ue.RetryAfter)
	assert.Contains(t, ue.Message, "slow down")
	assert.True(t, errors.Is(err, provider.ErrRateLimited))
}

func TestMapHTTPError_Overloaded_RetryAfterAlwaysParsed(t *testing.T) {
	// Even on 529 (not 429), overloaded_error should read Retry-After.
	body := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
	header := http.Header{"Retry-After": []string{"15"}}

	err := mapHTTPError("anthropic", 529, body, header)
	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.Equal(t, 15*time.Second, ue.RetryAfter)
	assert.True(t, errors.Is(err, provider.ErrUnavailable))
}

func TestMapHTTPError_MalformedBody_ClassifiesByStatus(t *testing.T) {
	err := mapHTTPError("anthropic", 401, []byte("internal-gateway-garbage"), nil)
	assert.True(t, errors.Is(err, provider.ErrAuth))
}

func TestMapHTTPError_LongNonJSONBodyTruncated(t *testing.T) {
	longBody := make([]byte, 500)
	for i := range longBody {
		longBody[i] = 'x'
	}
	err := mapHTTPError("anthropic", 500, longBody, nil)
	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.LessOrEqual(t, len(ue.Message), maxEmbeddedBodyLen+3)
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"-1", 0},
		{"abc", 0},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, parseRetryAfter(tc.in))
		})
	}
}

func TestMapRequestError(t *testing.T) {
	t.Run("canceled_passthrough", func(t *testing.T) {
		err := mapRequestError("anthropic", context.Canceled)
		assert.ErrorIs(t, err, context.Canceled)
		var ue *provider.UpstreamError
		assert.False(t, errors.As(err, &ue))
	})

	t.Run("deadline_exceeded_becomes_timeout", func(t *testing.T) {
		err := mapRequestError("anthropic", context.DeadlineExceeded)
		assert.True(t, errors.Is(err, provider.ErrTimeout))
		assert.True(t, provider.IsRetryable(err))
	})

	t.Run("generic_becomes_unavailable", func(t *testing.T) {
		err := mapRequestError("anthropic", errors.New("dial tcp: connection refused"))
		assert.True(t, errors.Is(err, provider.ErrUnavailable))
	})
}
