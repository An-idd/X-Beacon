package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/config"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/ratelimit"
	"github.com/An-idd/x-beacon/internal/router"
	"github.com/An-idd/x-beacon/internal/server"
	"github.com/An-idd/x-beacon/internal/storage"
)

// buildRouter constructs the Week 6 retry/failover/breaker layer. It pulls
// RetryPolicy + BreakerSettings from cfg.Router and wires them around the
// existing registry. The returned *router.Router is mandatory in
// server.Deps; callers using an empty registry still get a working router
// (it will just return ErrNoProviderForModel on every request).
func buildRouter(cfg *config.Config, reg *registry.Registry, logger *zap.Logger) *router.Router {
	policy := router.RetryPolicy{
		MaxRetries:  cfg.Router.Retry.MaxRetries,
		MaxTotal:    cfg.Router.Retry.MaxTotal,
		BaseBackoff: cfg.Router.Retry.BaseBackoff,
		MaxBackoff:  cfg.Router.Retry.MaxBackoff,
	}
	breaker := router.BreakerSettings{
		MaxRequests:  cfg.Router.Breaker.MaxRequests,
		Interval:     cfg.Router.Breaker.Interval,
		Timeout:      cfg.Router.Breaker.Timeout,
		FailureRatio: cfg.Router.Breaker.FailureRatio,
		MinRequests:  cfg.Router.Breaker.MinRequests,
	}
	return router.New(reg, policy, logger, router.WithBreakerSettings(breaker))
}

// loadPool builds the Postgres connection pool from cfg. Returns
// (nil, nil) when database.dsn is empty — that's the dev-mode signal
// "run without DB". Construction is lazy (pgx pool doesn't connect on
// build), so a returned non-nil pool may still fail Ping; that's
// loadAuth's job to detect and translate into the unauthenticated
// fallback.
func loadPool(ctx context.Context, cfg *config.Config, logger *zap.Logger) (*storage.Pool, error) {
	if cfg.Database.DSN == "" {
		logger.Warn("database.dsn is empty; gateway starts without a DB pool",
			zap.String("hint", "set database.dsn in config.yaml to enable Postgres-backed auth"))
		return nil, nil
	}
	return storage.NewPool(ctx, storage.Config{
		DSN:             cfg.Database.DSN,
		MaxConns:        cfg.Database.MaxOpenConns,
		MaxConnLifetime: cfg.Database.ConnMaxLifetime,
	})
}

// buildReadinessCheckers assembles the slice of dependency probes /readyz
// runs on every call. We add a checker only when the corresponding
// dependency was actually constructed; passing nil pool/rdb degrades to
// "no checks" and /readyz becomes a 200 (matches dev-mode where the
// gateway runs without DB/Redis).
func buildReadinessCheckers(pool *storage.Pool, rdb *redis.Client) []server.ReadinessChecker {
	var checkers []server.ReadinessChecker
	if pool != nil {
		checkers = append(checkers, server.ReadinessChecker{
			Name: "postgres",
			Check: func(ctx context.Context) error {
				return pool.Ping(ctx)
			},
		})
	}
	if rdb != nil {
		checkers = append(checkers, server.ReadinessChecker{
			Name: "redis",
			Check: func(ctx context.Context) error {
				return rdb.Ping(ctx).Err()
			},
		})
	}
	return checkers
}

// buildRateLimiter translates config.RateLimitRule → ratelimit.RuleConfig
// and constructs the Multi aggregator. Empty config or build failure
// returns nil — server treats that as "no rate-limiting" and the
// middleware short-circuits.
func buildRateLimiter(cfg *config.Config, rdb redis.UniversalClient, logger *zap.Logger) (*ratelimit.Multi, error) {
	if len(cfg.RateLimits) == 0 {
		logger.Info("no rate_limits configured; rate-limit middleware is a no-op")
		return nil, nil
	}

	rcs := make([]ratelimit.RuleConfig, len(cfg.RateLimits))
	for i, r := range cfg.RateLimits {
		rcs[i] = ratelimit.RuleConfig{
			Name:      r.Name,
			Algorithm: r.Algorithm,
			Rate:      r.Rate,
			Window:    r.Window,
			Limit:     r.Limit,
			Burst:     r.Burst,
			KeyBy:     r.KeyBy,
		}
	}
	rules, err := ratelimit.Build(rcs, rdb)
	if err != nil {
		return nil, err
	}
	logger.Info("rate-limit rules loaded", zap.Int("count", len(rules)))
	return ratelimit.NewMulti(rules...), nil
}

// loadRedis builds and pings the Redis client used by the auth cache
// (Step 4.4) and, later, by the rate-limit + semantic-cache layers
// (Week 5 / Week 9). Returns nil when Redis is unconfigured or
// unreachable — callers must treat nil as "no cache available" and
// degrade gracefully (auth.NewPostgres without the wrapper, etc.).
//
// Pinging at startup is a deliberate trade-off: a few hundred ms of
// boot latency in exchange for a single, clear log line that tells ops
// "cache is on" or "cache is off, here's why".
func loadRedis(ctx context.Context, cfg *config.Config, logger *zap.Logger) *redis.Client {
	if cfg.Redis.Addr == "" {
		logger.Warn("redis.addr is empty; auth cache disabled",
			zap.String("hint", "set redis.addr in config.yaml to enable caching"))
		return nil
	}
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
		PoolSize: cfg.Redis.PoolSize,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		logger.Warn("redis unreachable; auth cache disabled until /readyz turns green",
			zap.Error(err))
		_ = client.Close()
		return nil
	}
	logger.Info("redis ready", zap.String("addr", cfg.Redis.Addr))
	return client
}

// loadAuth wires the production Authenticator. From Step 4.3, that means
// Postgres-backed (the YAML auth.yaml path was removed).
//
// Dev-mode tolerance is preserved: if the DB is unreachable at startup,
// loadAuth logs a warning and returns (nil, nil) so the gateway boots
// without auth on /v1/*. Operators who require auth must monitor for
// the warn line; production deployments should fail readiness checks
// (Step 4.6 /readyz) while DB is down.
func loadAuth(ctx context.Context, pool *storage.Pool, logger *zap.Logger) (auth.Authenticator, error) {
	if pool == nil {
		logger.Warn("DB not configured; /v1/* will be unauthenticated",
			zap.String("hint", "set database.dsn in config.yaml to enable Postgres-backed auth"))
		return nil, nil
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		logger.Warn("DB unreachable at startup; /v1/* will be unauthenticated until /readyz turns green",
			zap.Error(err))
		return nil, nil
	}

	logger.Info("postgres auth ready")
	return auth.NewPostgres(pool), nil
}

// loadRegistry reads providers.yaml and constructs a Registry, tolerating
// the file's absence at startup with a clear warning. Any other error
// (permissions, malformed YAML, validation) is fatal.
//
// This stays in cmd/gateway because the absent-file warning is a
// startup-policy decision (zero-config dev experience), not server logic.
func loadRegistry(path string, logger *zap.Logger) (*registry.Registry, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("providers file not found; gateway starts with empty registry",
				zap.String("path", path),
				zap.String("hint", "copy configs/providers.example.yaml and set API keys"))
			return registry.NewEmpty(), nil
		}
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	reg, err := registry.Load(path)
	if err != nil {
		return nil, err
	}
	logger.Info("providers loaded",
		zap.Strings("names", reg.Names()),
		zap.Int("models", len(reg.AllModels())))
	return reg, nil
}
