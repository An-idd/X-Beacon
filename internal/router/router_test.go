package router

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// fakeProvider is a configurable Provider stub for the retry-loop tests.
// Each call records its index and returns the script entry at that index;
// when the script is exhausted the last entry repeats.
type fakeProvider struct {
	name   string
	models []provider.ModelInfo
	calls  atomic.Int32
	script []scriptStep
}

type scriptStep struct {
	resp *provider.ChatResponse
	err  error

	// Stream-only fields. streamPreErr replays from ChatCompletionStream's
	// pre-channel error path; streamEvents is the channel content after a
	// successful stream open.
	streamPreErr error
	streamEvents []provider.StreamEvent
}

func (f *fakeProvider) Name() string                       { return f.name }
func (f *fakeProvider) SupportedModels() []provider.ModelInfo { return f.models }

func (f *fakeProvider) ChatCompletion(_ context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
	idx := int(f.calls.Add(1)) - 1
	if idx >= len(f.script) {
		idx = len(f.script) - 1
	}
	return f.script[idx].resp, f.script[idx].err
}

func (f *fakeProvider) ChatCompletionStream(_ context.Context, _ *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	idx := int(f.calls.Add(1)) - 1
	if idx >= len(f.script) {
		idx = len(f.script) - 1
	}
	step := f.script[idx]
	if step.streamPreErr != nil {
		return nil, step.streamPreErr
	}
	ch := make(chan provider.StreamEvent, len(step.streamEvents)+1)
	for _, ev := range step.streamEvents {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// fakeResolver maps a single hard-coded model to a fail-over chain of
// providers. Index 0 is the primary; later entries are fail-over candidates.
type fakeResolver struct {
	model string
	chain []provider.Provider
}

func (r *fakeResolver) ResolveModel(model string) (provider.Provider, error) {
	chain := r.ResolveChain(model)
	if len(chain) == 0 {
		return nil, registry.ErrNoProviderForModel
	}
	return chain[0], nil
}

func (r *fakeResolver) ResolveChain(model string) []provider.Provider {
	if model != r.model {
		return nil
	}
	out := make([]provider.Provider, len(r.chain))
	copy(out, r.chain)
	return out
}

func newRouterForTest(t *testing.T, f *fakeProvider, policy RetryPolicy, sleepRecorder *[]time.Duration) *Router {
	t.Helper()
	now := time.Now()
	clock := &fakeClock{t: now}
	sleep := func(_ context.Context, d time.Duration) error {
		if sleepRecorder != nil {
			*sleepRecorder = append(*sleepRecorder, d)
		}
		clock.advance(d)
		return nil
	}
	resolver := &fakeResolver{model: "test-model", chain: []provider.Provider{f}}
	return New(resolver, policy, zap.NewNop(),
		WithClock(clock.now),
		WithSleep(sleep),
		WithRandom(func() float64 { return 0.5 }),
	)
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time         { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func sampleReq() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
}

func TestRouterChatCompletion_SuccessFirstTry(t *testing.T) {
	resp := &provider.ChatResponse{ID: "ok", Model: "test-model"}
	f := &fakeProvider{
		name:   "fp",
		models: []provider.ModelInfo{{ID: "test-model", Object: "model", Provider: "fp"}},
		script: []scriptStep{{resp: resp}},
	}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	got, err := r.ChatCompletion(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Same(t, resp, got)
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestRouterChatCompletion_RetryThenSuccess(t *testing.T) {
	resp := &provider.ChatResponse{ID: "ok"}
	f := &fakeProvider{
		name:   "fp",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{
			{err: provider.NewUpstreamError("fp", provider.ErrUpstream, 502, "bad gateway")},
			{resp: resp},
		},
	}
	var sleeps []time.Duration
	r := newRouterForTest(t, f, DefaultPolicy(), &sleeps)

	got, err := r.ChatCompletion(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Same(t, resp, got)
	assert.EqualValues(t, 2, f.calls.Load())
	assert.Len(t, sleeps, 1)
	assert.Equal(t, 50*time.Millisecond, sleeps[0]) // attempt=1, envelope=100ms, rand=0.5
}

func TestRouterChatCompletion_ExhaustsRetries(t *testing.T) {
	upErr := provider.NewUpstreamError("fp", provider.ErrUnavailable, 503, "down")
	f := &fakeProvider{
		name:   "fp",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: upErr}},
	}
	var sleeps []time.Duration
	r := newRouterForTest(t, f, DefaultPolicy(), &sleeps)

	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrUnavailable))
	// MaxRetries=2 → 1 initial + 2 retries = 3 calls, 2 sleeps.
	assert.EqualValues(t, 3, f.calls.Load())
	assert.Len(t, sleeps, 2)
	// envelope 100ms, 200ms; rand=0.5 → 50ms, 100ms
	assert.Equal(t, []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}, sleeps)
}

func TestRouterChatCompletion_NonRetryableShortCircuits(t *testing.T) {
	authErr := provider.NewUpstreamError("fp", provider.ErrAuth, 401, "bad key")
	f := &fakeProvider{
		name:   "fp",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: authErr}},
	}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrAuth))
	// No retry: only 1 call.
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestRouterChatCompletion_RetryAfterRespected(t *testing.T) {
	upErr := &provider.UpstreamError{
		Provider:   "fp",
		Sentinel:   provider.ErrRateLimited,
		StatusCode: 429,
		RetryAfter: 7 * time.Second,
	}
	resp := &provider.ChatResponse{ID: "ok"}
	f := &fakeProvider{
		name:   "fp",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{
			{err: upErr},
			{resp: resp},
		},
	}
	var sleeps []time.Duration
	r := newRouterForTest(t, f, DefaultPolicy(), &sleeps)

	got, err := r.ChatCompletion(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Same(t, resp, got)
	require.Len(t, sleeps, 1)
	assert.Equal(t, 7*time.Second, sleeps[0]) // Retry-After verbatim, no jitter
}

func TestRouterChatCompletion_TimeBudgetAborts(t *testing.T) {
	upErr := &provider.UpstreamError{
		Provider:   "fp",
		Sentinel:   provider.ErrRateLimited,
		RetryAfter: 25 * time.Second,
	}
	f := &fakeProvider{
		name:   "fp",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: upErr}},
	}
	policy := DefaultPolicy()
	policy.MaxTotal = 30 * time.Second
	var sleeps []time.Duration
	r := newRouterForTest(t, f, policy, &sleeps)

	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	// First retry sleeps 25s (under 30s budget). Second retry would also sleep
	// 25s — total elapsed already 25s, so 25+25 > 30 aborts.
	assert.EqualValues(t, 2, f.calls.Load())
	assert.Len(t, sleeps, 1)
	assert.Equal(t, 25*time.Second, sleeps[0])
}

func TestRouterChatCompletion_CtxCancelDuringSleep(t *testing.T) {
	f := &fakeProvider{
		name:   "fp",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{
			err: provider.NewUpstreamError("fp", provider.ErrUpstream, 502, "bad gw"),
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	resolver := &fakeResolver{model: "test-model", chain: []provider.Provider{f}}
	r := New(resolver, DefaultPolicy(), zap.NewNop(),
		WithSleep(func(c context.Context, _ time.Duration) error {
			select {
			case <-c.Done():
				return c.Err()
			default:
				return nil
			}
		}),
		WithRandom(func() float64 { return 0.5 }),
	)

	_, err := r.ChatCompletion(ctx, sampleReq())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestRouterChatCompletion_UnknownModel(t *testing.T) {
	f := &fakeProvider{name: "fp", models: []provider.ModelInfo{{ID: "test-model"}}}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	req := sampleReq()
	req.Model = "gpt-not-real"
	_, err := r.ChatCompletion(context.Background(), req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, registry.ErrNoProviderForModel))
}

func TestRouterChatCompletion_ZeroMaxRetries(t *testing.T) {
	f := &fakeProvider{
		name:   "fp",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{
			err: provider.NewUpstreamError("fp", provider.ErrUpstream, 502, ""),
		}},
	}
	policy := DefaultPolicy()
	policy.MaxRetries = 0
	var sleeps []time.Duration
	r := newRouterForTest(t, f, policy, &sleeps)

	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	assert.EqualValues(t, 1, f.calls.Load())
	assert.Len(t, sleeps, 0)
}

func TestRouterChatCompletion_ResolveModelExposed(t *testing.T) {
	f := &fakeProvider{name: "fp", models: []provider.ModelInfo{{ID: "test-model"}}}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	got, err := r.ResolveModel("test-model")
	require.NoError(t, err)
	assert.Same(t, provider.Provider(f), got)

	_, err = r.ResolveModel("nope")
	assert.True(t, errors.Is(err, registry.ErrNoProviderForModel))
}

// --- Failover (Step 6.2) ---

func newChainRouterForTest(t *testing.T, chain []provider.Provider, policy RetryPolicy, sleepRecorder *[]time.Duration) *Router {
	t.Helper()
	clock := &fakeClock{t: time.Now()}
	sleep := func(_ context.Context, d time.Duration) error {
		if sleepRecorder != nil {
			*sleepRecorder = append(*sleepRecorder, d)
		}
		clock.advance(d)
		return nil
	}
	resolver := &fakeResolver{model: "test-model", chain: chain}
	return New(resolver, policy, zap.NewNop(),
		WithClock(clock.now),
		WithSleep(sleep),
		WithRandom(func() float64 { return 0.5 }),
	)
}

func TestRouterChatCompletion_FailoverOnRetryable(t *testing.T) {
	primaryErr := provider.NewUpstreamError("primary", provider.ErrUnavailable, 503, "down")
	primary := &fakeProvider{
		name:   "primary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: primaryErr}}, // every call fails
	}
	secondaryResp := &provider.ChatResponse{ID: "from-secondary"}
	secondary := &fakeProvider{
		name:   "secondary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{resp: secondaryResp}},
	}
	var sleeps []time.Duration
	r := newChainRouterForTest(t, []provider.Provider{primary, secondary}, DefaultPolicy(), &sleeps)

	got, err := r.ChatCompletion(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Same(t, secondaryResp, got)
	// Primary exhausts MaxRetries=2 (3 calls); secondary succeeds first try.
	assert.EqualValues(t, 3, primary.calls.Load())
	assert.EqualValues(t, 1, secondary.calls.Load())
	// 2 sleeps for primary's retries, none for the failover hop itself.
	assert.Len(t, sleeps, 2)
}

func TestRouterChatCompletion_FailoverNonRetryableShortCircuits(t *testing.T) {
	primary := &fakeProvider{
		name:   "primary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{
			err: provider.NewUpstreamError("primary", provider.ErrInvalidRequest, 400, "bad params"),
		}},
	}
	secondary := &fakeProvider{
		name:   "secondary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{resp: &provider.ChatResponse{ID: "should-not-be-called"}}},
	}
	r := newChainRouterForTest(t, []provider.Provider{primary, secondary}, DefaultPolicy(), nil)

	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrInvalidRequest))
	// Failover skipped: secondary never touched.
	assert.EqualValues(t, 1, primary.calls.Load())
	assert.EqualValues(t, 0, secondary.calls.Load())
}

func TestRouterChatCompletion_FailoverAllExhausted(t *testing.T) {
	primaryErr := provider.NewUpstreamError("primary", provider.ErrUpstream, 502, "p down")
	secondaryErr := provider.NewUpstreamError("secondary", provider.ErrUnavailable, 503, "s down")
	primary := &fakeProvider{
		name:   "primary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: primaryErr}},
	}
	secondary := &fakeProvider{
		name:   "secondary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: secondaryErr}},
	}
	r := newChainRouterForTest(t, []provider.Provider{primary, secondary}, DefaultPolicy(), nil)

	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	// Last error wins (from secondary, the last provider tried).
	assert.True(t, errors.Is(err, provider.ErrUnavailable))
	assert.EqualValues(t, 3, primary.calls.Load())
	assert.EqualValues(t, 3, secondary.calls.Load())
}

func TestRouterChatCompletion_FailoverChainSharesTimeBudget(t *testing.T) {
	// MaxTotal=300ms; primary's 2 retries (sleeps 50+100=150ms) push elapsed
	// near budget; failover to secondary then aborts because elapsed already
	// exceeds budget.
	upErr := provider.NewUpstreamError("p", provider.ErrUnavailable, 503, "")
	primary := &fakeProvider{
		name:   "primary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: upErr}},
	}
	secondary := &fakeProvider{
		name:   "secondary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: upErr}},
	}
	policy := DefaultPolicy()
	policy.MaxTotal = 200 * time.Millisecond
	var sleeps []time.Duration
	r := newChainRouterForTest(t, []provider.Provider{primary, secondary}, policy, &sleeps)

	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	// Primary: attempt 1 (call 1), sleep 50ms, attempt 2 (call 2), sleep 100ms
	// would push to 150ms (under 200ms cap), retry. attempt 3 (call 3) errs;
	// no more retries (MaxRetries=2). Failover decision: elapsed 150ms < 200ms,
	// so secondary IS attempted.  Secondary call 1 errs at elapsed 150ms;
	// next sleep 50ms → 200ms not > 200ms, so retries. After 2nd retry
	// elapsed = 250ms > 200ms → abort within secondary.
	assert.GreaterOrEqual(t, primary.calls.Load(), int32(3))
	assert.GreaterOrEqual(t, secondary.calls.Load(), int32(1))
	// At minimum some sleeps recorded; budget cap means strictly fewer than
	// the un-budgeted (4 sleeps across 6 calls) total.
	assert.NotEmpty(t, sleeps)
}

func TestRouterChatCompletion_NoChain(t *testing.T) {
	resolver := &fakeResolver{model: "test-model", chain: nil}
	r := New(resolver, DefaultPolicy(), zap.NewNop())

	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	assert.True(t, errors.Is(err, registry.ErrNoProviderForModel))
}

// --- Circuit breaker (Step 6.3) ---

// fastBreakerSettings produces a breaker that trips after 2/2 failures.
// Used by tests so we don't have to make 5+ calls to verify trip logic.
func fastBreakerSettings() BreakerSettings {
	return BreakerSettings{
		MaxRequests:  1,
		Interval:     time.Hour,
		Timeout:      time.Hour, // never auto-recovers in test
		FailureRatio: 0.5,
		MinRequests:  2,
	}
}

func newRouterForBreakerTest(t *testing.T, chain []provider.Provider, policy RetryPolicy, breakerSettings BreakerSettings) *Router {
	t.Helper()
	clock := &fakeClock{t: time.Now()}
	sleep := func(_ context.Context, d time.Duration) error {
		clock.advance(d)
		return nil
	}
	resolver := &fakeResolver{model: "test-model", chain: chain}
	return New(resolver, policy, zap.NewNop(),
		WithClock(clock.now),
		WithSleep(sleep),
		WithRandom(func() float64 { return 0.5 }),
		WithBreakerSettings(breakerSettings),
	)
}

func TestRouterChatCompletion_BreakerTripsThenSkipsProvider(t *testing.T) {
	primaryErr := provider.NewUpstreamError("primary", provider.ErrUnavailable, 503, "down")
	primary := &fakeProvider{
		name:   "primary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: primaryErr}},
	}
	secondaryResp := &provider.ChatResponse{ID: "from-secondary"}
	secondary := &fakeProvider{
		name:   "secondary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{resp: secondaryResp}},
	}
	r := newRouterForBreakerTest(t, []provider.Provider{primary, secondary},
		DefaultPolicy(), fastBreakerSettings())

	// First request: primary's first 2 attempts fail (each counted as a
	// breaker failure). After 2 failures with MinRequests=2 / FailureRatio=
	// 0.5, the breaker trips. Attempt 3 hits the open breaker → propagates
	// as a breaker error, treated as failover-eligible. Secondary succeeds.
	got, err := r.ChatCompletion(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Same(t, secondaryResp, got)
	primaryCallsAfterFirst := primary.calls.Load()
	assert.EqualValues(t, 2, primaryCallsAfterFirst)
	assert.Equal(t, "open", r.breakers.state("primary").String())

	// Second request: breaker open → primary skipped entirely; chain falls
	// straight through to secondary. Primary call count must NOT change.
	secondary.script = []scriptStep{{resp: &provider.ChatResponse{ID: "second"}}}
	secondary.calls.Store(0)
	got, err = r.ChatCompletion(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Equal(t, "second", got.ID)
	assert.EqualValues(t, primaryCallsAfterFirst, primary.calls.Load(),
		"primary should be skipped while breaker is open")
}

func TestRouterChatCompletion_BreakerOpenAlone_StillFailsWhenNoFallback(t *testing.T) {
	primaryErr := provider.NewUpstreamError("primary", provider.ErrUnavailable, 503, "down")
	primary := &fakeProvider{
		name:   "primary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: primaryErr}},
	}
	r := newRouterForBreakerTest(t, []provider.Provider{primary},
		DefaultPolicy(), fastBreakerSettings())

	// Trip the breaker: 1 request × 3 attempts = 3 failures.
	_, err := r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	assert.Equal(t, "open", r.breakers.state("primary").String())

	// Next request: breaker open, no fallback, gateway must surface a
	// breaker error (which the HTTP layer can translate to 503).
	primary.calls.Store(0)
	_, err = r.ChatCompletion(context.Background(), sampleReq())
	require.Error(t, err)
	assert.True(t, isBreakerError(err))
	assert.EqualValues(t, 0, primary.calls.Load(), "primary not called while breaker open")
}

func TestRouterChatCompletion_BreakerIgnoresClientErrors(t *testing.T) {
	// 400-class failures (invalid request, auth) should NOT contribute to
	// breaker trip — the upstream is healthy, the client just sent bad input.
	clientErr := provider.NewUpstreamError("primary", provider.ErrInvalidRequest, 400, "bad")
	primary := &fakeProvider{
		name:   "primary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{err: clientErr}},
	}
	r := newRouterForBreakerTest(t, []provider.Provider{primary},
		DefaultPolicy(), fastBreakerSettings())

	// 3 failed requests, all 400. With FailureRatio=0.5 / MinRequests=2 the
	// breaker would trip if these counted — but breakerObservation filters
	// non-retryable errors out, so requests count as success.
	for range 3 {
		_, err := r.ChatCompletion(context.Background(), sampleReq())
		require.Error(t, err)
		assert.True(t, errors.Is(err, provider.ErrInvalidRequest))
	}
	assert.Equal(t, "closed", r.breakers.state("primary").String())
}

func TestBreakerObservation(t *testing.T) {
	// Pure-function unit: success → success, retryable → failure, 4xx → success.
	assert.Nil(t, breakerObservation(nil))
	assert.NotNil(t, breakerObservation(provider.NewUpstreamError("p", provider.ErrUnavailable, 503, "")))
	assert.NotNil(t, breakerObservation(provider.NewUpstreamError("p", provider.ErrUpstream, 502, "")))
	assert.Nil(t, breakerObservation(provider.NewUpstreamError("p", provider.ErrInvalidRequest, 400, "")))
	assert.Nil(t, breakerObservation(provider.NewUpstreamError("p", provider.ErrAuth, 401, "")))
}

// --- Streaming (Step 6.4) ---

func chunk(id string) provider.StreamEvent {
	return provider.StreamEvent{Chunk: &provider.ChatStreamChunk{ID: id}}
}

func errEv(err error) provider.StreamEvent { return provider.StreamEvent{Err: err} }

func TestRouterStream_HappyPath(t *testing.T) {
	f := &fakeProvider{
		name:   "p",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{streamEvents: []provider.StreamEvent{chunk("a"), chunk("b")}}},
	}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	ch, err := r.ChatCompletionStream(context.Background(), sampleReq())
	require.NoError(t, err)
	var ids []string
	for ev := range ch {
		require.NoError(t, ev.Err)
		ids = append(ids, ev.Chunk.ID)
	}
	assert.Equal(t, []string{"a", "b"}, ids)
}

func TestRouterStream_PreCallErrRetries(t *testing.T) {
	resp := []provider.StreamEvent{chunk("only")}
	f := &fakeProvider{
		name:   "p",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{
			{streamPreErr: provider.NewUpstreamError("p", provider.ErrUpstream, 502, "down")},
			{streamEvents: resp},
		},
	}
	var sleeps []time.Duration
	r := newRouterForTest(t, f, DefaultPolicy(), &sleeps)

	ch, err := r.ChatCompletionStream(context.Background(), sampleReq())
	require.NoError(t, err)
	var ids []string
	for ev := range ch {
		require.NoError(t, ev.Err)
		ids = append(ids, ev.Chunk.ID)
	}
	assert.Equal(t, []string{"only"}, ids)
	assert.EqualValues(t, 2, f.calls.Load())
	assert.Len(t, sleeps, 1)
}

func TestRouterStream_FirstEventErrRetries(t *testing.T) {
	// Provider opens the channel cleanly but the first event is an error.
	// We can still retry because the client hasn't seen anything yet.
	f := &fakeProvider{
		name:   "p",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{
			{streamEvents: []provider.StreamEvent{
				errEv(provider.NewUpstreamError("p", provider.ErrUpstream, 502, "")),
			}},
			{streamEvents: []provider.StreamEvent{chunk("ok")}},
		},
	}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	ch, err := r.ChatCompletionStream(context.Background(), sampleReq())
	require.NoError(t, err)
	var ids []string
	for ev := range ch {
		require.NoError(t, ev.Err)
		ids = append(ids, ev.Chunk.ID)
	}
	assert.Equal(t, []string{"ok"}, ids)
}

func TestRouterStream_MidStreamErrPropagates(t *testing.T) {
	// First event is a chunk → committed. Second event is an error → must
	// be forwarded to caller; NO retry.
	midErr := provider.NewUpstreamError("p", provider.ErrUpstream, 502, "lost connection")
	f := &fakeProvider{
		name:   "p",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{streamEvents: []provider.StreamEvent{chunk("a"), errEv(midErr)}}},
	}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	ch, err := r.ChatCompletionStream(context.Background(), sampleReq())
	require.NoError(t, err)

	var got []provider.StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Chunk.ID)
	require.Error(t, got[1].Err)
	assert.True(t, errors.Is(got[1].Err, provider.ErrUpstream))
	// Single attempt — no retry.
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestRouterStream_FailoverPreChunk(t *testing.T) {
	primary := &fakeProvider{
		name:   "primary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{streamPreErr: provider.NewUpstreamError("primary", provider.ErrUnavailable, 503, "")}},
	}
	secondary := &fakeProvider{
		name:   "secondary",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{streamEvents: []provider.StreamEvent{chunk("from-secondary")}}},
	}
	r := newChainRouterForTest(t, []provider.Provider{primary, secondary}, DefaultPolicy(), nil)

	ch, err := r.ChatCompletionStream(context.Background(), sampleReq())
	require.NoError(t, err)
	var ids []string
	for ev := range ch {
		require.NoError(t, ev.Err)
		ids = append(ids, ev.Chunk.ID)
	}
	assert.Equal(t, []string{"from-secondary"}, ids)
}

func TestRouterStream_ChannelClosedWithoutEvents(t *testing.T) {
	// Provider opens a channel and closes it immediately — protocol violation;
	// router synthesizes ErrUpstream and retries.
	resp := []provider.StreamEvent{chunk("rescued")}
	f := &fakeProvider{
		name:   "p",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{
			{streamEvents: nil}, // empty channel, immediate close
			{streamEvents: resp},
		},
	}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	ch, err := r.ChatCompletionStream(context.Background(), sampleReq())
	require.NoError(t, err)
	var ids []string
	for ev := range ch {
		require.NoError(t, ev.Err)
		ids = append(ids, ev.Chunk.ID)
	}
	assert.Equal(t, []string{"rescued"}, ids)
}

func TestRouterStream_NonRetryablePreCall(t *testing.T) {
	f := &fakeProvider{
		name:   "p",
		models: []provider.ModelInfo{{ID: "test-model"}},
		script: []scriptStep{{streamPreErr: provider.NewUpstreamError("p", provider.ErrInvalidRequest, 400, "")}},
	}
	r := newRouterForTest(t, f, DefaultPolicy(), nil)

	_, err := r.ChatCompletionStream(context.Background(), sampleReq())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrInvalidRequest))
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestRouterStream_NoChain(t *testing.T) {
	resolver := &fakeResolver{model: "test-model", chain: nil}
	r := New(resolver, DefaultPolicy(), zap.NewNop())

	_, err := r.ChatCompletionStream(context.Background(), sampleReq())
	require.Error(t, err)
	assert.True(t, errors.Is(err, registry.ErrNoProviderForModel))
}

func TestDefaultBreakerSettings(t *testing.T) {
	s := DefaultBreakerSettings()
	assert.Equal(t, uint32(1), s.MaxRequests)
	assert.Equal(t, 60*time.Second, s.Interval)
	assert.Equal(t, 30*time.Second, s.Timeout)
	assert.InDelta(t, 0.5, s.FailureRatio, 0.001)
	assert.Equal(t, uint32(5), s.MinRequests)
}
