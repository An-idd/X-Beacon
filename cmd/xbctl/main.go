// Command xbctl is the operations CLI for X-BEACON.
//
// Subcommands:
//
//	keygen     Generate and persist a new API key
//	keylist    List configured API keys
//	keyrevoke  Mark a key revoked
//	migrate    Apply schema migrations (up | down | version)
//
// Run `xbctl <subcommand> -h` for per-subcommand flags.
//
// xbctl shares config.yaml with the gateway: by default it reads the
// Postgres DSN from the same file, so a single source of truth governs
// both processes. `--dsn` overrides for ops scripts.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/An-idd/x-beacon/pkg/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run is the testable core. The first argument is the subcommand name;
// remaining args are forwarded to the per-subcommand parser.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage(stdout)
		if len(args) == 0 {
			return fmt.Errorf("subcommand required")
		}
		return nil
	}
	if args[0] == "-version" || args[0] == "--version" {
		fmt.Fprintf(stdout, "xbctl %s (commit %s, built %s)\n",
			version.Version, version.Commit, version.BuildTime)
		return nil
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "keygen":
		return runKeygen(rest, stdout)
	case "keylist":
		return runKeylist(rest, stdout)
	case "keyrevoke":
		return runKeyrevoke(rest, stdout)
	case "migrate":
		return runMigrate(rest, stdout)
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `xbctl — X-BEACON operations CLI

Usage:
  xbctl <subcommand> [flags]

Subcommands:
  keygen     Generate and persist a new API key (prints secret ONCE)
  keylist    List configured API keys
  keyrevoke  Mark an API key as revoked
  migrate    Apply schema migrations: up | down | version

Common flags (all DB-touching subcommands):
  -config PATH    Read database.dsn from this config.yaml (default: configs/config.yaml)
  -dsn URL        Use this DSN directly; overrides -config

Run "xbctl <subcommand> -h" for per-subcommand details.
`)
}
