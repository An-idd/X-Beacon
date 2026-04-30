package observability

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTimeSeries_NilReceiverIsNoOp(t *testing.T) {
	var ts *TimeSeries
	ts.Record(200)
	assert.Nil(t, ts.Snapshot())
	assert.True(t, ts.Started().IsZero())
}

func TestTimeSeries_RecordSplitsSuccessAndError(t *testing.T) {
	ts := NewTimeSeries()

	ts.Record(200)
	ts.Record(201)
	ts.Record(404)
	ts.Record(503)

	pts := ts.Snapshot()
	// Find the current bucket (last point).
	cur := pts[len(pts)-1]
	assert.Equal(t, 2, cur.Success)
	assert.Equal(t, 2, cur.Error)
}

func TestTimeSeries_SnapshotReturns60Points(t *testing.T) {
	ts := NewTimeSeries()
	ts.Record(200)

	pts := ts.Snapshot()
	assert.Len(t, pts, 60)
	// Oldest first → newest last.
	for i := 1; i < len(pts); i++ {
		assert.True(t, pts[i].T.After(pts[i-1].T) || pts[i].T.Equal(pts[i-1].T),
			"snapshot[%d].T (%v) must not be before snapshot[%d].T (%v)",
			i, pts[i].T, i-1, pts[i-1].T)
	}
}

func TestTimeSeries_StaleSlotResets(t *testing.T) {
	// Inject a fake clock so we can advance time deterministically.
	ts := NewTimeSeries()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	ts.now = func() time.Time { return base }

	ts.Record(200)
	ts.Record(500)

	// Jump exactly 60 minutes — same ring index, but different bucket.
	ts.now = func() time.Time { return base.Add(60 * time.Minute) }
	ts.Record(200)

	pts := ts.Snapshot()
	// Newest point should reflect the new minute's single success
	// only — not the carry-over from 60 minutes ago.
	cur := pts[len(pts)-1]
	assert.Equal(t, 1, cur.Success)
	assert.Equal(t, 0, cur.Error,
		"slot must reset when a different bucket lands on the same ring index")
}

func TestTimeSeries_ConcurrentSafe(t *testing.T) {
	ts := NewTimeSeries()
	const writers = 16
	const opsPer = 1000

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < opsPer; i++ {
				if (seed+i)%3 == 0 {
					ts.Record(500)
				} else {
					ts.Record(200)
				}
			}
		}(w)
	}
	wg.Wait()

	pts := ts.Snapshot()
	cur := pts[len(pts)-1]
	total := cur.Success + cur.Error
	assert.Equal(t, writers*opsPer, total,
		"all ops must be recorded; lost a count means race")
}
