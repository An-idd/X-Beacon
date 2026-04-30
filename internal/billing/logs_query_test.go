package billing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestListLogs_NilPool(t *testing.T) {
	now := time.Now()
	_, _, err := ListLogs(context.Background(), nil, LogQuery{
		Start: now.Add(-time.Hour), End: now,
	})
	assert.Error(t, err, "nil pool with valid window should error")
}

func TestListLogs_WindowValidation(t *testing.T) {
	now := time.Now()

	cases := map[string]LogQuery{
		"zero start": {End: now},
		"zero end":   {Start: now},
		"start == end": {
			Start: now, End: now,
		},
		"start > end": {
			Start: now, End: now.Add(-time.Hour),
		},
	}
	for label, q := range cases {
		t.Run(label, func(t *testing.T) {
			_, _, err := ListLogs(context.Background(), nil, q)
			assert.ErrorIs(t, err, ErrLogWindowInvalid, "%s should fail with ErrLogWindowInvalid", label)
		})
	}
}

func TestListLogs_WindowTooWide(t *testing.T) {
	now := time.Now()
	_, _, err := ListLogs(context.Background(), nil, LogQuery{
		Start: now.Add(-8 * 24 * time.Hour),
		End:   now,
	})
	assert.ErrorIs(t, err, ErrLogWindowTooWide)
}

func TestListLogs_RejectsBadStatusBucket(t *testing.T) {
	// Use a non-nil pool placeholder to bypass the nil check; the
	// status validation runs before the DB query so we never get
	// to a real call. We can't easily inject a pool here without
	// integration; rely on the fact that status validation runs
	// before any pool dereference by NOT exercising it — instead
	// assert via integration tests in the admin suite.
	t.Skip("status bucket validation covered by admin integration tests (gated XBEACON_TEST_DSN)")
}

func TestPreviewID(t *testing.T) {
	assert.Equal(t, "01234567", previewID("0123456789abcdef"))
	assert.Equal(t, "abc", previewID("abc"))
	assert.Equal(t, "", previewID(""))
}
