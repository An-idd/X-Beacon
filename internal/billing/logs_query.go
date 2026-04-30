package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/An-idd/x-beacon/internal/storage"
)

// LogQuery shapes the parameters accepted by ListLogs. All time
// fields are required (Start / End); the window must be ≤ 7d so an
// admin can't accidentally scan the entire ledger.
//
// APIKeyIDPrefix is a prefix match (`LIKE prefix || '%'`); empty
// disables the filter. Same for Model (exact match; empty disables).
// StatusBucket is one of "", "2xx", "4xx", "5xx" — chosen at the
// HTTP layer to avoid leaking integer status semantics into the
// table query.
type LogQuery struct {
	Start          time.Time
	End            time.Time
	APIKeyIDPrefix string
	Model          string
	StatusBucket   string // "" | "2xx" | "4xx" | "5xx"
	Limit          int    // clamped to [1, 200]
	Offset         int    // >= 0
}

// LogRow is one projected request_logs entry. Notably absent: any
// prompt / message / response content. Per docs/runbook.md the
// schema doesn't store those fields at all (defense in depth) —
// this projection just makes the no-leak property explicit at the
// API boundary.
type LogRow struct {
	RequestID        string
	StartedAt        time.Time
	APIKeyID         string
	APIKeyIDPreview  string
	Model            string
	Provider         string
	Status           int
	LatencyMs        int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostMicro        int64
	Currency         string
	ErrorCode        string
	Streamed         bool
}

const (
	maxLogWindow = 7 * 24 * time.Hour
	maxLogLimit  = 200
	defLogLimit  = 50
)

// ErrLogWindowTooWide is returned when End - Start > 7d. Surface as
// 400 at the HTTP layer so the admin gets a clear "narrow your
// window" hint instead of a slow query.
var ErrLogWindowTooWide = errors.New("billing: log window must be <= 7d")

// ErrLogWindowInvalid covers Start >= End and zero-time inputs.
var ErrLogWindowInvalid = errors.New("billing: log window invalid (require start < end, both non-zero)")

// ListLogs returns matching rows + total count. Total uses
// `COUNT(*) OVER ()` for v0.1 simplicity; switch to a separate
// COUNT or cursor pagination if a single window ever exceeds
// ~100k rows in practice (track via the `total > 100000` warn
// header at the HTTP boundary).
func ListLogs(ctx context.Context, pool *storage.Pool, q LogQuery) ([]LogRow, int, error) {
	// Validate inputs before pool — surface bad requests as bad
	// requests, not "internal config" errors.
	if q.Start.IsZero() || q.End.IsZero() || !q.Start.Before(q.End) {
		return nil, 0, ErrLogWindowInvalid
	}
	if q.End.Sub(q.Start) > maxLogWindow {
		return nil, 0, ErrLogWindowTooWide
	}
	if pool == nil {
		return nil, 0, errors.New("billing: pool is nil")
	}
	if q.Limit <= 0 || q.Limit > maxLogLimit {
		q.Limit = defLogLimit
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	// Boring positional WHERE assembly — readable beats clever for
	// admin tooling.
	where := "WHERE started_at >= $1 AND started_at < $2"
	args := []any{q.Start, q.End}

	if q.APIKeyIDPrefix != "" {
		args = append(args, q.APIKeyIDPrefix)
		where += fmt.Sprintf(" AND api_key_id LIKE $%d || '%%'", len(args))
	}
	if q.Model != "" {
		args = append(args, q.Model)
		where += fmt.Sprintf(" AND model = $%d", len(args))
	}
	switch q.StatusBucket {
	case "":
		// no-op: all statuses
	case "2xx":
		where += " AND status >= 200 AND status < 300"
	case "4xx":
		where += " AND status >= 400 AND status < 500"
	case "5xx":
		where += " AND status >= 500 AND status < 600"
	default:
		return nil, 0, fmt.Errorf("billing: invalid status_bucket %q (allowed: 2xx|4xx|5xx)", q.StatusBucket)
	}

	args = append(args, q.Limit, q.Offset)
	sql := fmt.Sprintf(`
		SELECT request_id, started_at, COALESCE(api_key_id, ''), model, provider,
		       status, latency_ms, prompt_tokens, completion_tokens, total_tokens,
		       total_micro, currency, COALESCE(error_code, ''), streamed,
		       COUNT(*) OVER () AS total
		  FROM request_logs
		  %s
		  ORDER BY started_at DESC
		  LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("billing: query request_logs: %w", err)
	}
	defer rows.Close()

	var (
		out   []LogRow
		total int
	)
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(
			&r.RequestID, &r.StartedAt, &r.APIKeyID, &r.Model, &r.Provider,
			&r.Status, &r.LatencyMs, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.CostMicro, &r.Currency, &r.ErrorCode, &r.Streamed,
			&total); err != nil {
			return nil, 0, fmt.Errorf("billing: scan request_logs row: %w", err)
		}
		r.APIKeyIDPreview = previewID(r.APIKeyID)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("billing: iterate request_logs: %w", err)
	}
	return out, total, nil
}

func previewID(id string) string {
	const w = 8
	if len(id) <= w {
		return id
	}
	return id[:w]
}
