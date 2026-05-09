package server

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// adminStatsSummaryHandler exposes the dashboard's number-cards
// payload. Cached at the collector layer (~5s) so polling at WebUI
// cadence collapses to ~12 Gather() calls per minute regardless of
// how many admins are connected.
//
// Window semantics: process_uptime, NOT a real 24h rolling window.
// The `since` field tells the WebUI when to anchor the "since X"
// copy. See docs/webui-backend-tasks.md task 3.
func adminStatsSummaryHandler(stats *observability.StatsCollector, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		summary, err := stats.Summary()
		if err != nil {
			logger.Error("admin stats summary failed",
				zap.String("req_id", reqID), zap.Error(err))
			writeError(w, mappedError{
				Status: http.StatusInternalServerError, Type: "internal_error",
				Message: "Failed to compute stats summary",
			}, reqID)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(summary)
	}
}

// adminStatsCacheHandler exposes /admin/stats/cache — the v0.2 §3.2
// projection of cache hit rates + semantic threshold + similarity
// bucket distribution. Cached at the collector layer (5s).
func adminStatsCacheHandler(stats *observability.CacheStatsCollector, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		out, err := stats.Stats()
		if err != nil {
			logger.Error("admin cache stats failed",
				zap.String("req_id", reqID), zap.Error(err))
			writeError(w, mappedError{
				Status: http.StatusInternalServerError, Type: "internal_error",
				Message: "Failed to compute cache stats",
			}, reqID)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// adminStatsTimeseriesHandler returns 60 chronological one-minute
// points ending at the current minute. v0.1 only supports
// metric=qps; query-string parsing is permissive (extra params
// ignored) so the WebUI can evolve forward without server changes.
//
// Cold ring positions surface as zero-count points at the bucket's
// nominal start; the WebUI uses `since` from /admin/stats/summary
// to know where the meaningful data begins.
func adminStatsTimeseriesHandler(ts *observability.TimeSeries) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// metric / window / step are advisory in v0.1; we always
		// return qps over 1h with 1m step. Echo back what was
		// requested so the WebUI can display "as of <window>".
		metric := r.URL.Query().Get("metric")
		if metric == "" {
			metric = "qps"
		}

		points := ts.Snapshot()
		if points == nil {
			points = []observability.TimePoint{}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metric": metric,
			"window": "1h",
			"step":   "1m",
			"points": points,
		})
	}
}
