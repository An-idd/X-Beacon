package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/An-idd/x-beacon/internal/storage"
)

func runMigrate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stdout)
	in := registerDSNFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("migrate: subaction required (up | down | version)")
	}
	if len(rest) > 1 {
		return fmt.Errorf("migrate: unexpected arguments after %q", rest[0])
	}

	dsn, err := resolveDSN(in)
	if err != nil {
		return err
	}

	switch rest[0] {
	case "up":
		if err := storage.MigrateUp(dsn); err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		fmt.Fprintln(stdout, "Schema is at head.")
		return nil
	case "down":
		if err := storage.MigrateDown(dsn); err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		fmt.Fprintln(stdout, "Rolled back one migration.")
		return nil
	case "version":
		v, dirty, err := storage.MigrateVersion(dsn)
		if err != nil {
			return fmt.Errorf("migrate version: %w", err)
		}
		fmt.Fprintf(stdout, "version=%d dirty=%v\n", v, dirty)
		return nil
	default:
		return fmt.Errorf("migrate: unknown subaction %q (want: up | down | version)", rest[0])
	}
}
