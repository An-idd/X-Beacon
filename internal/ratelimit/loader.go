package ratelimit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// RuleConfig is the package-local declarative form of a rate-limit rule.
// main translates config.RateLimitRule → RuleConfig at the assembly
// boundary, keeping internal/ratelimit free of upstream dependencies.
type RuleConfig struct {
	// Name is a stable identifier surfaced in 429 responses and metrics.
	// Must be unique within a Build call.
	Name string

	// Algorithm picks the implementation: "memory_bucket" or "redis_window".
	Algorithm string

	// Rate is parsed by parseRate (e.g. "100/s", "60/m", "1000/h").
	// Used by memory_bucket.
	Rate string

	// Burst is the memory_bucket bucket capacity. 0 falls back to
	// (Rate per second) which gives a 1-second-equivalent burst.
	Burst int

	// Window is the redis_window rolling window. Must be > 0 for that algo.
	Window time.Duration

	// Limit is the redis_window cap. Must be >= 1 for that algo.
	Limit int

	// KeyBy is the list of dimensions this rule keys on. Empty = global.
	// Allowed values: "api_key", "model".
	KeyBy []string
}

// Build translates a slice of RuleConfig into runtime Rules. All errors
// are aggregated via errors.Join so a misconfigured config.yaml shows
// every problem at once (matches registry.Load semantics).
//
// rdb may be nil; rules using "redis_window" then fail validation with
// a clear message rather than panicking at first request.
func Build(configs []RuleConfig, rdb redis.UniversalClient) ([]*Rule, error) {
	if len(configs) == 0 {
		return nil, nil
	}

	out := make([]*Rule, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))

	var errs []error
	for i, rc := range configs {
		if rc.Name == "" {
			errs = append(errs, fmt.Errorf("rules[%d]: name is required", i))
			continue
		}
		if _, dup := seen[rc.Name]; dup {
			errs = append(errs, fmt.Errorf("rules[%d]: duplicate name %q", i, rc.Name))
			continue
		}
		seen[rc.Name] = struct{}{}

		kbs, err := parseKeyBys(rc.KeyBy)
		if err != nil {
			errs = append(errs, fmt.Errorf("rules[%d] %q: %w", i, rc.Name, err))
			continue
		}

		var lim Limiter
		switch rc.Algorithm {
		case "memory_bucket":
			lim, err = buildMemoryBucket(rc)
		case "redis_window":
			lim, err = buildRedisWindow(rc, rdb)
		default:
			err = fmt.Errorf("unknown algorithm %q (want memory_bucket | redis_window)", rc.Algorithm)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("rules[%d] %q: %w", i, rc.Name, err))
			continue
		}

		out = append(out, &Rule{Name: rc.Name, KeyBy: kbs, Limiter: lim})
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("ratelimit: build failed: %w", errors.Join(errs...))
	}
	return out, nil
}

// parseKeyBys validates and converts string dimensions into typed KeyBy
// constants. Unknown dimensions error rather than silently treat as global.
func parseKeyBys(in []string) ([]KeyBy, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]KeyBy, 0, len(in))
	for _, s := range in {
		switch s {
		case "api_key":
			out = append(out, KeyByAPIKey)
		case "model":
			out = append(out, KeyByModel)
		default:
			return nil, fmt.Errorf("unknown key_by %q (want api_key | model | empty for global)", s)
		}
	}
	return out, nil
}

// parseRate accepts "<n>/<unit>" with unit ∈ {s, m, h}. Returns tokens/s.
func parseRate(s string) (rate.Limit, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, fmt.Errorf("rate %q: expected form <n>/<s|m|h>", s)
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("rate %q: numerator must be a positive integer", s)
	}
	var per time.Duration
	switch strings.TrimSpace(parts[1]) {
	case "s":
		per = time.Second
	case "m":
		per = time.Minute
	case "h":
		per = time.Hour
	default:
		return 0, fmt.Errorf("rate %q: unit must be s | m | h", s)
	}
	return rate.Limit(float64(n) / per.Seconds()), nil
}

func buildMemoryBucket(rc RuleConfig) (Limiter, error) {
	r, err := parseRate(rc.Rate)
	if err != nil {
		return nil, err
	}
	burst := rc.Burst
	if burst == 0 {
		// Default burst = 1 second of average rate, rounded up to >=1.
		// Rationale: short-burst tolerance equal to the steady-state
		// allowance is the OpenAI-ish norm and matches typical k8s probe
		// expectations. Configurable when this default doesn't fit.
		burst = int(float64(r))
		if burst < 1 {
			burst = 1
		}
	}
	return NewMemoryBucket(MemoryBucketConfig{Rate: r, Burst: burst})
}

func buildRedisWindow(rc RuleConfig, rdb redis.UniversalClient) (Limiter, error) {
	if rdb == nil {
		return nil, errors.New("redis_window requires Redis (set redis.addr in config.yaml)")
	}
	if rc.Limit < 1 {
		return nil, errors.New("redis_window: limit must be >= 1")
	}
	if rc.Window <= 0 {
		return nil, errors.New("redis_window: window must be > 0")
	}
	return NewRedisWindow(RedisWindowConfig{Client: rdb, Limit: rc.Limit, Window: rc.Window})
}
