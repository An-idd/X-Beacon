package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/An-idd/x-beacon/internal/storage"
)

func runKeyrevoke(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keyrevoke", flag.ContinueOnError)
	fs.SetOutput(stdout)
	in := registerDSNFlags(fs)
	id := fs.String("id", "", "ID of the key to revoke (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *id == "" {
		return errors.New("--id is required")
	}

	dsn, err := resolveDSN(in)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn, MaxConns: 2})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// Conditional update: only flip revoked_at if it's still NULL. This
	// makes the operation idempotent against repeats and lets us
	// distinguish "no such key" from "already revoked" via rows-affected.
	tag, err := pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`,
		*id,
	)
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}

	switch tag.RowsAffected() {
	case 1:
		fmt.Fprintf(stdout, "Revoked key %s.\n", *id)
		fmt.Fprintln(stdout, "Note: cached principals may persist up to one positive-TTL window.")
		return nil
	case 0:
		// Distinguish: row exists but already revoked vs. row doesn't exist.
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys WHERE id = $1)`, *id).Scan(&exists); err == nil {
			if exists {
				fmt.Fprintf(stdout, "Key %s is already revoked; no change.\n", *id)
				return nil
			}
		}
		return fmt.Errorf("no key with id %q", *id)
	default:
		// PK uniqueness should make >1 impossible; if it happens, surface loudly.
		return fmt.Errorf("revoke: unexpected rows affected: %d", tag.RowsAffected())
	}
}
