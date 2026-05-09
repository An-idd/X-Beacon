// Package audit records mutating admin actions to a forensic log.
// Designed for "who changed pricing for gpt-4o on Saturday" rather
// than for high-volume request telemetry — that lives in
// request_logs (see internal/billing).
//
// Recording is synchronous: a successful Record means the row hit
// the DB before the admin handler returned 200. Volume is low
// (operator clicks per day), so we don't need a worker queue like
// billing does. Sync also lets the handler propagate audit failures
// rather than dropping them silently.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/An-idd/x-beacon/internal/storage"
)

// Action identifies what was done. Strings are stable; the WebUI
// joins on them. Format: <category>.<verb>.
type Action string

const (
	ActionKeyCreate     Action = "key.create"
	ActionKeyRevoke     Action = "key.revoke"
	ActionPricingUpsert Action = "pricing.upsert"
	ActionPricingDelete Action = "pricing.delete"
)

// Entry is one audit row's input. Recorder.Record takes this
// directly; query handlers project to a similar shape on output.
type Entry struct {
	ActorID    string
	ActorLabel string
	Action     Action
	TargetType string
	TargetID   string
	Metadata   any // JSON-marshalled into the JSONB column
	RequestID  string
}

// Row is the projected shape returned by Query. Mirrors Entry but
// with the DB-supplied fields added.
type Row struct {
	ID         int64
	OccurredAt time.Time
	ActorID    string
	ActorLabel string
	Action     string
	TargetType string
	TargetID   string
	Metadata   json.RawMessage
	RequestID  string
}

// QueryOpts shapes /admin/audit reads. Range is required (start <
// end, ≤ 31d to bound table scans even though the table is small).
type QueryOpts struct {
	Start      time.Time
	End        time.Time
	ActorID    string // exact match; "" disables
	Action     string // exact match; "" disables
	TargetType string // exact match; "" disables
	Limit      int
	Offset     int
}

const (
	maxAuditWindow = 31 * 24 * time.Hour
	maxAuditLimit  = 200
	defAuditLimit  = 50
)

var (
	ErrNotConfigured     = errors.New("audit: recorder not wired (no DB pool)")
	ErrWindowInvalid     = errors.New("audit: window invalid (start < end, both non-zero)")
	ErrWindowTooWide     = errors.New("audit: window must be <= 31d")
	ErrInvalidAction     = errors.New("audit: action must be in 'category.verb' form")
	ErrMissingActorOrTgt = errors.New("audit: actor_id and target are required")
)

// Recorder writes audit rows. Interface lets tests stub it without a
// DB. Production wires Postgres via NewPostgres.
type Recorder interface {
	Record(ctx context.Context, e Entry) error
	Query(ctx context.Context, opts QueryOpts) ([]Row, int, error)
}

// nopRecorder discards all entries. Used when no DB is configured
// (dev mode) so admin handlers can call Record unconditionally.
type nopRecorder struct{}

func (nopRecorder) Record(context.Context, Entry) error { return nil }
func (nopRecorder) Query(context.Context, QueryOpts) ([]Row, int, error) {
	return nil, 0, ErrNotConfigured
}

// Nop returns a Recorder that drops all writes. Used in dev mode
// without a DB; admin handlers can call Record() unconditionally
// without nil-checks.
func Nop() Recorder { return nopRecorder{} }

// Postgres is the Recorder backed by storage.Pool.
type Postgres struct {
	pool *storage.Pool
}

func NewPostgres(pool *storage.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// Record validates and inserts one audit row. Validation runs before
// the pool nil check so callers see the most specific error.
func (p *Postgres) Record(ctx context.Context, e Entry) error {
	if e.ActorID == "" || e.TargetID == "" || e.TargetType == "" {
		return ErrMissingActorOrTgt
	}
	action := string(e.Action)
	dot := false
	for i := 0; i < len(action); i++ {
		if action[i] == '.' && i > 0 && i < len(action)-1 {
			dot = true
			break
		}
	}
	if !dot {
		return ErrInvalidAction
	}
	if p == nil || p.pool == nil {
		return ErrNotConfigured
	}

	var meta []byte
	if e.Metadata != nil {
		raw, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("audit: marshal metadata: %w", err)
		}
		meta = raw
	}

	const q = `
		INSERT INTO admin_audit_logs (actor_id, actor_label, action, target_type, target_id, metadata, request_id)
		     VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))`
	_, err := p.pool.Exec(ctx, q,
		e.ActorID, e.ActorLabel, action, e.TargetType, e.TargetID, meta, e.RequestID,
	)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// Query returns matching rows + total. Same `count(*) OVER ()`
// pattern as billing.ListLogs — the table is small enough that the
// extra cost is fine through 100k rows.
func (p *Postgres) Query(ctx context.Context, opts QueryOpts) ([]Row, int, error) {
	if opts.Start.IsZero() || opts.End.IsZero() || !opts.Start.Before(opts.End) {
		return nil, 0, ErrWindowInvalid
	}
	if opts.End.Sub(opts.Start) > maxAuditWindow {
		return nil, 0, ErrWindowTooWide
	}
	if p == nil || p.pool == nil {
		return nil, 0, ErrNotConfigured
	}
	if opts.Limit <= 0 || opts.Limit > maxAuditLimit {
		opts.Limit = defAuditLimit
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}

	where := "WHERE occurred_at >= $1 AND occurred_at < $2"
	args := []any{opts.Start, opts.End}
	if opts.ActorID != "" {
		args = append(args, opts.ActorID)
		where += fmt.Sprintf(" AND actor_id = $%d", len(args))
	}
	if opts.Action != "" {
		args = append(args, opts.Action)
		where += fmt.Sprintf(" AND action = $%d", len(args))
	}
	if opts.TargetType != "" {
		args = append(args, opts.TargetType)
		where += fmt.Sprintf(" AND target_type = $%d", len(args))
	}
	args = append(args, opts.Limit, opts.Offset)
	sql := fmt.Sprintf(`
		SELECT id, occurred_at, actor_id, actor_label, action, target_type, target_id,
		       COALESCE(metadata, 'null'::jsonb), COALESCE(request_id, ''),
		       COUNT(*) OVER () AS total
		  FROM admin_audit_logs
		  %s
		  ORDER BY occurred_at DESC
		  LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: query: %w", err)
	}
	defer rows.Close()

	var (
		out   []Row
		total int
	)
	for rows.Next() {
		var r Row
		if err := rows.Scan(
			&r.ID, &r.OccurredAt, &r.ActorID, &r.ActorLabel, &r.Action,
			&r.TargetType, &r.TargetID, &r.Metadata, &r.RequestID, &total,
		); err != nil {
			return nil, 0, fmt.Errorf("audit: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("audit: iterate: %w", err)
	}
	return out, total, nil
}
