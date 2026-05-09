package audit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Hermetic tests for the validation paths. The Postgres-backed
// path is covered indirectly by /admin/keys + /admin/pricing
// integration tests when XBEACON_TEST_DSN is set.

func TestPostgres_RecordRejectsInvalidActor(t *testing.T) {
	p := NewPostgres(nil) // pool deliberately nil; validation runs first
	err := p.Record(context.Background(), Entry{
		ActorID:    "",
		Action:     ActionKeyCreate,
		TargetType: "api_key",
		TargetID:   "ak_x",
	})
	assert.ErrorIs(t, err, ErrMissingActorOrTgt)
}

func TestPostgres_RecordRejectsBadAction(t *testing.T) {
	p := NewPostgres(nil)
	err := p.Record(context.Background(), Entry{
		ActorID:    "ak_admin",
		Action:     "bareword",
		TargetType: "api_key",
		TargetID:   "ak_x",
	})
	assert.ErrorIs(t, err, ErrInvalidAction)
}

func TestPostgres_QueryWindowValidation(t *testing.T) {
	p := NewPostgres(nil)
	now := time.Now()

	cases := map[string]QueryOpts{
		"zero start":  {End: now},
		"zero end":    {Start: now},
		"start == end": {Start: now, End: now},
		"start > end": {Start: now, End: now.Add(-time.Hour)},
	}
	for label, opts := range cases {
		t.Run(label, func(t *testing.T) {
			_, _, err := p.Query(context.Background(), opts)
			assert.ErrorIs(t, err, ErrWindowInvalid, label)
		})
	}
}

func TestPostgres_QueryWindowTooWide(t *testing.T) {
	p := NewPostgres(nil)
	now := time.Now()
	_, _, err := p.Query(context.Background(), QueryOpts{
		Start: now.Add(-32 * 24 * time.Hour),
		End:   now,
	})
	assert.ErrorIs(t, err, ErrWindowTooWide)
}

func TestNop_RecordIsNoOp(t *testing.T) {
	r := Nop()
	assert.NoError(t, r.Record(context.Background(), Entry{
		ActorID: "x", Action: ActionKeyCreate, TargetType: "t", TargetID: "y",
	}))
}

func TestNop_QueryReturnsNotConfigured(t *testing.T) {
	r := Nop()
	_, _, err := r.Query(context.Background(), QueryOpts{
		Start: time.Now().Add(-time.Hour), End: time.Now(),
	})
	assert.ErrorIs(t, err, ErrNotConfigured)
}
