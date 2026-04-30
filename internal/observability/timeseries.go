package observability

import (
	"sync"
	"time"
)

// TimeSeries is a 60-slot, 1-minute-bucketed in-memory ring buffer
// of (success, error) request counts. Sized for the WebUI dashboard's
// "last hour" view; not a long-term store (use Prometheus + Grafana).
//
// Trade-offs accepted:
//   - Resets on process restart. The /admin/stats endpoints expose
//     `since` so the WebUI knows where the data starts.
//   - Per-request lock acquisition is fine at 5000 RPS — sync.Mutex
//     is uncontested in the common case; a write per-second-bucket
//     is the only contended path.
//   - 60 slots × ~32 bytes = 2 KB. Negligible.
//
// Concurrency: safe for many writers + many readers. The mutex
// protects both write (Record) and read (Snapshot) paths.
type TimeSeries struct {
	started time.Time
	slotDur time.Duration

	mu    sync.Mutex
	slots [tsSlotCount]tsSlot
	now   func() time.Time // hookable for tests
}

type tsSlot struct {
	start   time.Time // start of this minute bucket; zero means "unused"
	success int
	error   int
}

const (
	tsSlotCount = 60
	tsSlotDur   = time.Minute
)

// TimePoint is one minute of the snapshot. Empty slots (process
// hasn't been up that long, or the bucket genuinely had no traffic)
// surface as zeroed counts at the bucket's nominal start time.
type TimePoint struct {
	T       time.Time `json:"t"`
	Success int       `json:"success"`
	Error   int       `json:"error"`
}

// NewTimeSeries returns an empty buffer scoped to one-minute slots.
// The startedAt is captured here so /admin/stats/summary can report
// "data only since this point".
func NewTimeSeries() *TimeSeries {
	return &TimeSeries{
		started: time.Now(),
		slotDur: tsSlotDur,
		now:     time.Now,
	}
}

// Started returns the construction time of this buffer. Used by the
// HTTP layer to populate `since` in stats responses.
func (t *TimeSeries) Started() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.started
}

// Record bumps the slot for the current minute. status >= 400 is
// counted as an error (matches /admin/stats/summary's bucketing).
//
// Nil receiver is a no-op, mirroring Metrics.* helpers — keeps the
// chat handler's hot path branch-free.
func (t *TimeSeries) Record(status int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	bucket := now.Truncate(t.slotDur)
	idx := slotIndex(bucket, t.slotDur)
	s := &t.slots[idx]

	// Re-initialize the slot if it's stale (a different minute is
	// hashing to the same index — happens once per ring rotation).
	if !s.start.Equal(bucket) {
		*s = tsSlot{start: bucket}
	}
	if status >= 400 {
		s.error++
	} else {
		s.success++
	}
}

// Snapshot returns 60 chronologically-ordered points ending at the
// current minute. Slots that don't have data (cold ring positions)
// surface as zero-count points at the bucket's nominal start.
func (t *TimeSeries) Snapshot() []TimePoint {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now().Truncate(t.slotDur)
	out := make([]TimePoint, tsSlotCount)
	// Walk from oldest (now - 59min) to newest (now), inclusive.
	for i := 0; i < tsSlotCount; i++ {
		bucket := now.Add(time.Duration(-(tsSlotCount - 1 - i)) * t.slotDur)
		idx := slotIndex(bucket, t.slotDur)
		s := t.slots[idx]
		if !s.start.Equal(bucket) {
			out[i] = TimePoint{T: bucket}
			continue
		}
		out[i] = TimePoint{T: s.start, Success: s.success, Error: s.error}
	}
	return out
}

// slotIndex maps a bucket-aligned time to its slot in the ring. The
// ring length is tsSlotCount; minute since unix epoch mod 60 is the
// position. This makes the mapping stateless — no write pointer to
// maintain or race against.
func slotIndex(bucket time.Time, slotDur time.Duration) int {
	return int((bucket.Unix() / int64(slotDur.Seconds())) % tsSlotCount)
}
