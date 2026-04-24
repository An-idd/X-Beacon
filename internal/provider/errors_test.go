package provider

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamError_ErrorFormat(t *testing.T) {
	tests := []struct {
		name   string
		err    *UpstreamError
		wantIn []string
	}{
		{
			name:   "status_and_message",
			err:    NewUpstreamError("openai", ErrRateLimited, 429, "quota exceeded"),
			wantIn: []string{"openai", "rate limited", "429", "quota exceeded"},
		},
		{
			name:   "status_only",
			err:    NewUpstreamError("anthropic", ErrUpstream, 500, ""),
			wantIn: []string{"anthropic", "upstream error", "500"},
		},
		{
			name:   "message_only",
			err:    NewUpstreamError("deepseek", ErrInvalidRequest, 0, "bad schema"),
			wantIn: []string{"deepseek", "invalid request", "bad schema"},
		},
		{
			name:   "bare",
			err:    NewUpstreamError("openai", ErrTimeout, 0, ""),
			wantIn: []string{"openai", "timeout"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.err.Error()
			for _, sub := range tc.wantIn {
				assert.Contains(t, s, sub)
			}
		})
	}
}

func TestUpstreamError_IsMatchesSentinel(t *testing.T) {
	err := NewUpstreamError("openai", ErrRateLimited, 429, "")
	assert.True(t, errors.Is(err, ErrRateLimited))
	assert.False(t, errors.Is(err, ErrAuth))

	// Wrapping should also propagate.
	wrapped := fmt.Errorf("calling openai: %w", err)
	assert.True(t, errors.Is(wrapped, ErrRateLimited))
}

func TestUpstreamError_AsExtraction(t *testing.T) {
	err := NewUpstreamError("openai", ErrRateLimited, 429, "slow down")
	err.RetryAfter = 5 * time.Second
	wrapped := fmt.Errorf("ctx: %w", err)

	var ue *UpstreamError
	require.True(t, errors.As(wrapped, &ue))
	assert.Equal(t, "openai", ue.Provider)
	assert.Equal(t, 429, ue.StatusCode)
	assert.Equal(t, 5*time.Second, ue.RetryAfter)
	assert.Equal(t, "slow down", ue.Message)
}

func TestIsRetryable(t *testing.T) {
	cases := map[error]bool{
		ErrRateLimited:    true,
		ErrUpstream:       true,
		ErrUnavailable:    true,
		ErrTimeout:        true,
		ErrAuth:           false,
		ErrContextLength:  false,
		ErrInvalidRequest: false,
		errors.New("unrelated random error"): false,
		nil: false,
	}
	for err, want := range cases {
		got := IsRetryable(err)
		assert.Equalf(t, want, got, "IsRetryable(%v) = %v, want %v", err, got, want)
	}
}

func TestIsRetryable_WithUpstreamWrapper(t *testing.T) {
	// A wrapped UpstreamError should still match the category.
	rl := NewUpstreamError("openai", ErrRateLimited, 429, "")
	wrapped := fmt.Errorf("attempt 2: %w", rl)
	assert.True(t, IsRetryable(wrapped))

	authErr := NewUpstreamError("openai", ErrAuth, 401, "")
	wrappedAuth := fmt.Errorf("startup: %w", authErr)
	assert.False(t, IsRetryable(wrappedAuth))
}
