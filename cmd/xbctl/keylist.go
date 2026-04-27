package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/An-idd/x-beacon/internal/storage"
)

// keyRow is the projected shape returned by keylist. The full key_hash
// is included so ops can correlate Redis cache entries (auth:k:<hex>) with
// rows in the table — handy when debugging "why is this key still cached".
type keyRow struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyHashHex string     `json:"key_hash_hex"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func runKeylist(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keylist", flag.ContinueOnError)
	fs.SetOutput(stdout)
	in := registerDSNFlags(fs)
	asJSON := fs.Bool("json", false, "emit JSON instead of a text table")
	includeRevoked := fs.Bool("all", false, "include revoked keys (default: active only)")
	if err := fs.Parse(args); err != nil {
		return err
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

	q := `SELECT id, name, key_hash, created_at, last_used_at, revoked_at
	        FROM api_keys`
	if !*includeRevoked {
		q += ` WHERE revoked_at IS NULL`
	}
	q += ` ORDER BY created_at DESC`

	rows, err := pool.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()

	var out []keyRow
	for rows.Next() {
		var r keyRow
		var hash []byte
		if err := rows.Scan(&r.ID, &r.Name, &hash, &r.CreatedAt, &r.LastUsedAt, &r.RevokedAt); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		r.KeyHashHex = hex.EncodeToString(hash)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	if *asJSON {
		return writeKeysJSON(stdout, out)
	}
	return writeKeysTable(stdout, out)
}

func writeKeysJSON(w io.Writer, rows []keyRow) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if rows == nil {
		// Force `[]` not `null` so consumers don't choke.
		rows = []keyRow{}
	}
	return enc.Encode(rows)
}

func writeKeysTable(w io.Writer, rows []keyRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tHASH\tCREATED\tLAST USED\tREVOKED")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s…\t%s\t%s\t%s\n",
			r.ID,
			r.Name,
			shortHash(r.KeyHashHex),
			formatTime(&r.CreatedAt),
			formatTime(r.LastUsedAt),
			formatTime(r.RevokedAt),
		)
	}
	if len(rows) == 0 {
		fmt.Fprintln(tw, "(no keys)")
	}
	return tw.Flush()
}

// shortHash trims the SHA-256 hex to the first 12 chars; keeps the table
// narrow without losing enough entropy to identify the row in a Redis
// SCAN debugging session.
func shortHash(h string) string {
	const w = 12
	if len(h) <= w {
		return h
	}
	return h[:w]
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
