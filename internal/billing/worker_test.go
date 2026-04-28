package billing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewWorker_RequiresPool(t *testing.T) {
	_, err := NewWorker(nil, nil, DefaultWorkerConfig(), zap.NewNop())
	require.Error(t, err)
}

func TestDefaultWorkerConfig(t *testing.T) {
	cfg := DefaultWorkerConfig()
	assert.Equal(t, 10000, cfg.BufferSize)
	assert.Equal(t, 2, cfg.Workers)
	assert.Equal(t, 5*time.Second, cfg.FlushTimeout)
}

func TestNullableString(t *testing.T) {
	assert.Nil(t, nullableString(""))
	assert.Equal(t, "x", nullableString("x"))
}

// --- DB-gated integration tests ---

func TestWorker_EnqueueAndPersist(t *testing.T) {
	pool := integrationPool(t)

	pricing := NewPricingCache(pool, zap.NewNop())
	require.NoError(t, pricing.Reload(context.Background()))

	w, err := NewWorker(pool, pricing, WorkerConfig{
		BufferSize: 100, Workers: 1, FlushTimeout: 2 * time.Second,
	}, zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	now := time.Now()
	ev := Event{
		StartedAt:        now,
		RequestID:        "req-test-1",
		APIKeyID:         "key-test",
		Provider:         "openai-mock",
		Model:            "gpt-4o",
		PromptTokens:     1000,
		CompletionTokens: 500,
		LatencyMs:        42,
		Status:           200,
		Streamed:         false,
	}
	assert.True(t, w.Enqueue(ev))

	// Wait for the row to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.WrittenCount() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.Stop(context.Background())

	assert.EqualValues(t, 1, w.WrittenCount())
	assert.EqualValues(t, 0, w.DroppedCount())

	// Verify the row was actually inserted with correct cost math.
	// gpt-4o: input 5000 micro/1k * 1000 prompt = 5000; output 15000/1k * 500 = 7500.
	var inMicro, outMicro, totMicro int64
	var currency string
	row := pool.QueryRow(context.Background(),
		`SELECT input_micro, output_micro, total_micro, currency
		   FROM request_logs WHERE request_id = $1`, "req-test-1")
	require.NoError(t, row.Scan(&inMicro, &outMicro, &totMicro, &currency))
	assert.Equal(t, int64(5000), inMicro)
	assert.Equal(t, int64(7500), outMicro)
	assert.Equal(t, int64(12500), totMicro)
	assert.Equal(t, "USD", currency)
}

func TestWorker_EnqueueDropsOnFullBuffer(t *testing.T) {
	pool := integrationPool(t)

	w, err := NewWorker(pool, nil, WorkerConfig{
		BufferSize: 2, Workers: 1, FlushTimeout: 2 * time.Second,
	}, zap.NewNop())
	require.NoError(t, err)
	// Don't Start — channel won't drain. Buffer fills up quickly.

	for i := 0; i < 5; i++ {
		w.Enqueue(Event{StartedAt: time.Now(), RequestID: "drop", Model: "any", Status: 200})
	}
	// First 2 enqueued (into buffer). Last 3 dropped.
	assert.GreaterOrEqual(t, w.DroppedCount(), int64(3))
	assert.LessOrEqual(t, w.DroppedCount(), int64(5))
}

func TestWorker_StopFlushesPendingEvents(t *testing.T) {
	pool := integrationPool(t)
	pricing := NewPricingCache(pool, zap.NewNop())
	require.NoError(t, pricing.Reload(context.Background()))

	w, err := NewWorker(pool, pricing, WorkerConfig{
		BufferSize: 100, Workers: 1, FlushTimeout: 5 * time.Second,
	}, zap.NewNop())
	require.NoError(t, err)
	w.Start(context.Background())

	// Enqueue a batch, then stop immediately.
	const N = 20
	for i := 0; i < N; i++ {
		w.Enqueue(Event{
			StartedAt: time.Now(),
			RequestID: "flush-test", Model: "gpt-4o-mini", Status: 200,
		})
	}
	w.Stop(context.Background())

	// All events should be persisted (FlushTimeout was generous).
	assert.GreaterOrEqual(t, w.WrittenCount(), int64(N))
}

func TestWorker_EnsurePartitionIdempotent(t *testing.T) {
	pool := integrationPool(t)
	w, err := NewWorker(pool, nil, DefaultWorkerConfig(), zap.NewNop())
	require.NoError(t, err)

	ts := time.Now()
	require.NoError(t, w.ensurePartition(context.Background(), ts))
	// Second call hits the cache, no DDL.
	require.NoError(t, w.ensurePartition(context.Background(), ts))

	// Different month → another partition.
	require.NoError(t, w.ensurePartition(context.Background(), ts.AddDate(0, -1, 0)))
}

func TestWorker_NonBlockingUnderConcurrentEnqueue(t *testing.T) {
	// Race detector + parallel writers verifies Enqueue is lock-free
	// from the producer's POV (channel send is the only contention).
	pool := integrationPool(t)
	w, err := NewWorker(pool, nil, WorkerConfig{
		BufferSize: 1000, Workers: 4, FlushTimeout: 5 * time.Second,
	}, zap.NewNop())
	require.NoError(t, err)
	w.Start(context.Background())
	defer w.Stop(context.Background())

	const goroutines = 16
	const perG = 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				w.Enqueue(Event{
					StartedAt: time.Now(),
					RequestID: "concurrent",
					Model:     "gpt-4o-mini", Status: 200,
				})
			}
		}()
	}
	// 1s timeout so a regression that re-introduces a producer-side
	// blocking call would surface as a hang.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Enqueue blocked under concurrent producers")
	}

	// Drain.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.WrittenCount()+w.DroppedCount() >= goroutines*perG {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.EqualValues(t, goroutines*perG, w.WrittenCount()+w.DroppedCount())
}

