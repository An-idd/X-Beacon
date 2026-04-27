// Package storage owns the gateway's PostgreSQL connection pool and the
// utilities that query it. Migrations live under storage/migrations and
// are embedded into the binary; see migrations.go (Step 4.2).
//
// The package follows two project rules:
//   - No ORM (CLAUDE.md): pgx + hand-written SQL.
//   - Config types are defined here, not imported from internal/config —
//     keeps storage usable from cmd/xbctl, tests, etc. main translates
//     config.DatabaseConfig → storage.Config at the assembly boundary.
package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config describes how to build the pool. Defaults are applied for
// zero-valued fields so callers can pass a partial config without
// repeating sensible defaults at every call site.
type Config struct {
	// DSN is a libpq-style connection string. Required.
	DSN string

	// MaxConns caps the pool size. <=0 falls back to pgx's default (4 or
	// the GOMAXPROCS-derived value, whichever is larger).
	MaxConns int

	// MinConns is the minimum number of idle connections kept warm.
	// <=0 leaves pgx's default of 0.
	MinConns int

	// MaxConnLifetime rotates connections after this age. 0 = no rotation.
	MaxConnLifetime time.Duration

	// MaxConnIdleTime closes idle connections after this duration. 0 = pgx default (30m).
	MaxConnIdleTime time.Duration
}

// Pool wraps *pgxpool.Pool with helpers the gateway uses (Ping for
// /readyz, structured close that survives nil receivers).
//
// Embedding *pgxpool.Pool exposes the pgx surface for callers that need
// direct query access — `storage.Pool.Query(ctx, ...)` works the same as
// `pgxpool.Pool.Query`.
type Pool struct {
	*pgxpool.Pool
}

// NewPool validates cfg and returns a *Pool. The pool is created lazily —
// no actual connection happens until first use — so this call succeeds
// even when PostgreSQL is unreachable. Use Ping to verify connectivity.
//
// Returns an error only when the DSN is malformed or pgx rejects the
// pool config (e.g. negative MaxConns). Network failures surface later
// via the first query / Ping.
func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.DSN == "" {
		return nil, errors.New("storage: Config.DSN is required")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("storage: parse DSN: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = int32(cfg.MaxConns)
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = int32(cfg.MinConns)
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("storage: build pool: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// Ping verifies the database is reachable. Used by the readiness probe
// (/readyz, Step 4.6). Wraps pgx's Ping mostly so callers don't have to
// import pgx types just to check liveness.
func (p *Pool) Ping(ctx context.Context) error {
	if p == nil || p.Pool == nil {
		return errors.New("storage: pool is nil")
	}
	return p.Pool.Ping(ctx)
}

// Close releases pool resources. Safe to call on a nil pool.
func (p *Pool) Close() {
	if p == nil || p.Pool == nil {
		return
	}
	p.Pool.Close()
}
