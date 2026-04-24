package openai

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

func TestMapStatusToSentinel(t *testing.T) {
	tests := []struct {
		status int
		code   string
		want   error
	}{
		{401, "", provider.ErrAuth},
		{403, "", provider.ErrAuth},
		{429, "", provider.ErrRateLimited},
		{400, "context_length_exceeded", provider.ErrContextLength},
		{400, "invalid_api_key", provider.ErrInvalidRequest},
		{400, "", provider.ErrInvalidRequest},
		{404, "", provider.ErrInvalidRequest},
		{422, "", provider.ErrInvalidRequest},
		{408, "", provider.ErrTimeout},
		{503, "", provider.ErrUnavailable},
		{500, "", provider.ErrUpstream},
		{502, "", provider.ErrUpstream},
		{504, "", provider.ErrUpstream},
		{418, "", provider.ErrUpstream}, // unknown 4xx
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%d_%s", tc.status, tc.code), func(t *testing.T) {
			got := mapStatusToSentinel(tc.status, tc.code)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMapHTTPError_JSONBody(t *testing.T) {
	body := []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	header := http.Header{"Retry-After": []string{"30"}}

	err := mapHTTPError("openai-primary", 429, body, header)
	require.Error(t, err)

	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.Equal(t, "openai-primary", ue.Provider)
	assert.Equal(t, 429, ue.StatusCode)
	assert.Equal(t, 30*time.Second, ue.RetryAfter)
	assert.Contains(t, ue.Message, "Rate limit exceeded")
	assert.True(t, errors.Is(err, provider.ErrRateLimited))
}

func TestMapHTTPError_NonJSONBody_TruncatedMessage(t *testing.T) {
	// Large non-JSON body should be truncated.
	longBody := make([]byte, 500)
	for i := range longBody {
		longBody[i] = 'x'
	}
	err := mapHTTPError("openai", 500, longBody, nil)

	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.LessOrEqual(t, len(ue.Message), maxEmbeddedBodyLen+3) // +3 for "..."
	assert.True(t, errors.Is(err, provider.ErrUpstream))
}

func TestMapHTTPError_ContextLengthFromCode(t *testing.T) {
	body := []byte(`{"error":{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	err := mapHTTPError("openai", 400, body, nil)
	assert.True(t, errors.Is(err, provider.ErrContextLength))
	assert.False(t, errors.Is(err, provider.ErrInvalidRequest))
}

func TestMapHTTPError_MalformedJSON_StillClassifies(t *testing.T) {
	// Body isn't JSON; classification must still work from status alone.
	err := mapHTTPError("openai", 401, []byte("internal gateway garbage"), nil)
	assert.True(t, errors.Is(err, provider.ErrAuth))
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"0", 0},
		{"-1", 0},                   // negative rejected
		{"abc", 0},                  // garbage
		{"3600", 3600 * time.Second}, // one hour is realistic
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, parseRetryAfter(tc.in))
		})
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future)
	// Allow a wide tolerance because of clock drift / scheduling.
	assert.InDelta(t, 45*time.Second, d, float64(5*time.Second))

	past := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
	assert.Equal(t, time.Duration(0), parseRetryAfter(past))
}

func TestMapRequestError(t *testing.T) {
	t.Run("canceled_passthrough", func(t *testing.T) {
		err := mapRequestError("openai", context.Canceled)
		assert.ErrorIs(t, err, context.Canceled)
		// Must NOT be wrapped as UpstreamError — ctx.Canceled is non-retryable
		// and callers inspect it directly.
		var ue *provider.UpstreamError
		assert.False(t, errors.As(err, &ue))
	})

	t.Run("deadline_exceeded_becomes_timeout", func(t *testing.T) {
		err := mapRequestError("openai", context.DeadlineExceeded)
		assert.True(t, errors.Is(err, provider.ErrTimeout))
		assert.True(t, provider.IsRetryable(err))
	})

	t.Run("generic_network_becomes_unavailable", func(t *testing.T) {
		err := mapRequestError("openai", errors.New("dial tcp: connection refused"))
		assert.True(t, errors.Is(err, provider.ErrUnavailable))
		assert.True(t, provider.IsRetryable(err))
	})

	t.Run("wrapped_deadline_still_recognized", func(t *testing.T) {
		wrapped := fmt.Errorf("transport: %w", context.DeadlineExceeded)
		err := mapRequestError("openai", wrapped)
		assert.True(t, errors.Is(err, provider.ErrTimeout))
	})
}
