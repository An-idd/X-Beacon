package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/server/middleware"
	"github.com/An-idd/x-beacon/internal/storage"
)

// adminLogsHandler returns the GET /admin/logs handler. The window is
// hard-capped at 7d (enforced inside billing.ListLogs); this prevents
// a misclick on the WebUI from scanning the entire ledger.
//
// The response intentionally omits prompt / message content. The
// schema doesn't store those fields, so this is mostly belt-and-
// braces — but the explicit DTO makes the no-leak property auditable
// at the API boundary.
func adminLogsHandler(pool *storage.Pool, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		q := r.URL.Query()

		query, mapped, ok := parseLogQuery(q)
		if !ok {
			writeError(w, mapped, reqID)
			return
		}

		rows, total, err := billing.ListLogs(r.Context(), pool, query)
		if err != nil {
			switch {
			case errors.Is(err, billing.ErrLogWindowInvalid):
				writeError(w, mappedError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Code: "invalid_window", Message: err.Error(),
				}, reqID)
			case errors.Is(err, billing.ErrLogWindowTooWide):
				writeError(w, mappedError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Code: "window_too_wide", Message: "Window must be <= 7d; narrow start/end and retry",
				}, reqID)
			default:
				logger.Error("admin logs query failed",
					zap.String("req_id", reqID), zap.Error(err))
				writeError(w, mappedError{
					Status: http.StatusInternalServerError, Type: "internal_error",
					Message: "Failed to query logs",
				}, reqID)
			}
			return
		}

		items := make([]logDTO, 0, len(rows))
		for _, row := range rows {
			items = append(items, dtoFromLogRow(row))
		}

		// Warn header when the unfiltered count is large enough that
		// COUNT(*) OVER () starts costing real time. WebUI surfaces
		// this as "results may be slow; narrow your filters".
		if total > 100000 {
			w.Header().Set("X-X-Beacon-Logs-Warn", "wide_window")
		}

		nextOffset := query.Offset + len(items)
		if nextOffset >= total {
			nextOffset = -1 // sentinel: no more pages
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object":      "list",
			"items":       items,
			"total":       total,
			"next_offset": nextOffset,
		})
	}
}

// logDTO is the on-the-wire shape. Field order mirrors WebUI display
// (most-clicked first) so JSON inspection in DevTools reads naturally.
type logDTO struct {
	RequestID        string    `json:"request_id"`
	StartedAt        time.Time `json:"started_at"`
	APIKeyID         string    `json:"api_key_id"`
	APIKeyIDPreview  string    `json:"api_key_id_preview"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	Status           int       `json:"status"`
	LatencyMs        int       `json:"latency_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	CostMicro        int64     `json:"cost_micro"`
	Currency         string    `json:"currency"`
	ErrorCode        string    `json:"error_code,omitempty"`
	Streamed         bool      `json:"streamed"`
}

func dtoFromLogRow(r billing.LogRow) logDTO {
	return logDTO{
		RequestID:        r.RequestID,
		StartedAt:        r.StartedAt,
		APIKeyID:         r.APIKeyID,
		APIKeyIDPreview:  r.APIKeyIDPreview,
		Model:            r.Model,
		Provider:         r.Provider,
		Status:           r.Status,
		LatencyMs:        r.LatencyMs,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
		CostMicro:        r.CostMicro,
		Currency:         r.Currency,
		ErrorCode:        r.ErrorCode,
		Streamed:         r.Streamed,
	}
}

// parseLogQuery validates query string and returns either a populated
// LogQuery or an HTTP-shaped error. start / end are required (RFC3339);
// limit / offset have defaults; status accepts the bucket-string form
// chosen at the URL layer.
func parseLogQuery(q map[string][]string) (billing.LogQuery, mappedError, bool) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}

	startRaw := get("start")
	endRaw := get("end")
	if startRaw == "" || endRaw == "" {
		return billing.LogQuery{}, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "missing_window", Message: "Both 'start' and 'end' (RFC3339) are required",
		}, false
	}
	start, err := time.Parse(time.RFC3339, startRaw)
	if err != nil {
		return billing.LogQuery{}, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_start", Message: "Field 'start' must be RFC3339",
		}, false
	}
	end, err := time.Parse(time.RFC3339, endRaw)
	if err != nil {
		return billing.LogQuery{}, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_end", Message: "Field 'end' must be RFC3339",
		}, false
	}

	out := billing.LogQuery{
		Start:          start,
		End:            end,
		APIKeyIDPrefix: get("api_key"),
		Model:          get("model"),
		StatusBucket:   get("status"),
	}
	// "all" is a UI nicety; treat it the same as omitted.
	if out.StatusBucket == "all" {
		out.StatusBucket = ""
	}

	if v := get("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 {
			return billing.LogQuery{}, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "invalid_limit", Message: "Field 'limit' must be a positive integer (max 200)",
			}, false
		}
		out.Limit = n
	}
	if v := get("offset"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			return billing.LogQuery{}, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "invalid_offset", Message: "Field 'offset' must be a non-negative integer",
			}, false
		}
		out.Offset = n
	}

	return out, mappedError{}, true
}
