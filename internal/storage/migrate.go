package storage

import (
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // register pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// migrationsFS embeds the SQL migration files so the binary is fully
// self-contained — `xbctl migrate up` works on a host without the
// repo present.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is the path inside migrationsFS that holds the .sql files.
const migrationsDir = "migrations"

// MigrateUp applies all pending migrations against the given DSN. The
// migrate library uses its own database connection (separate from
// storage.Pool) because it needs a serializable advisory lock that
// pgxpool's Prepare-cached driver doesn't expose.
//
// dsn is the same `postgres://...` URL used elsewhere; the scheme is
// rewritten to `pgx5://` internally so golang-migrate routes to the
// pgx v5 driver registered above.
//
// Returns nil when the schema is already at head (migrate's ErrNoChange
// is treated as success, matching `migrate up` CLI semantics).
func MigrateUp(dsn string) error {
	return migrateRun(dsn, func(m *migrate.Migrate) error {
		err := m.Up()
		if err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
		return nil
	})
}

// MigrateDown rolls back the most recent migration. Used by ops in
// emergency or by the test suite to reset between integration runs.
//
// Down on an empty schema is a no-op (ErrNoChange treated as success).
func MigrateDown(dsn string) error {
	return migrateRun(dsn, func(m *migrate.Migrate) error {
		err := m.Steps(-1)
		if err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
		return nil
	})
}

// MigrateVersion returns the current schema version (0 if nothing applied)
// and a dirty flag (true when a previous migration crashed mid-flight).
// Used by /readyz to refuse traffic if the schema is dirty.
func MigrateVersion(dsn string) (version uint, dirty bool, err error) {
	err = migrateRun(dsn, func(m *migrate.Migrate) error {
		v, d, vErr := m.Version()
		if errors.Is(vErr, migrate.ErrNilVersion) {
			version, dirty = 0, false
			return nil
		}
		version, dirty = v, d
		return vErr
	})
	return version, dirty, err
}

// migrateRun centralizes the open / defer-close pattern; callers supply
// the action (Up/Down/Version) to run between.
func migrateRun(dsn string, action func(*migrate.Migrate) error) error {
	src, err := iofs.New(migrationsFS, migrationsDir)
	if err != nil {
		return fmt.Errorf("storage: build migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, toMigrateURL(dsn))
	if err != nil {
		return fmt.Errorf("storage: open migrate: %w", err)
	}
	defer func() {
		// Both errors are best-effort: srcErr only fires on FS handle leaks
		// (impossible with embed); dbErr is duplicated by action's error
		// already, so silencing here avoids confusing double-reports.
		_, _ = m.Close()
	}()

	return action(m)
}

// toMigrateURL rewrites a `postgres://` or `postgresql://` DSN to the
// `pgx5://` scheme that golang-migrate's pgx v5 driver registers under.
// Anything else is passed through untouched (lets ops use a literal
// pgx5:// URL if they prefer).
func toMigrateURL(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	default:
		return dsn
	}
}
