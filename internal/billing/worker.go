package billing

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/storage"
)

// Event is one billable request. Constructed by the chat handler at
// response time and enqueued (non-blocking) on Worker. All token /
// cost fields default to zero — a non-priced model emits an event with
// zero cost, which is still useful for QPS / latency reporting.
type Event struct {
	StartedAt        time.Time
	RequestID        string
	APIKeyID         string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	LatencyMs        int
	Status           int
	Streamed         bool
	ErrorCode        string
}

// WorkerConfig configures buffer + worker count. Zero values fall back
// to sensible defaults inside NewWorker.
type WorkerConfig struct {
	// BufferSize is the channel capacity. When the channel is full,
	// Enqueue drops the event and increments DroppedCount; this keeps
	// the chat hot path zero-blocking.
	BufferSize int

	// Workers is the number of consumer goroutines. >1 lets a slow DB
	// not stall the queue; the order of inserted rows is no longer
	// strictly start-time monotonic but reports use started_at, not id.
	Workers int

	// FlushTimeout caps how long Stop waits for the drain on shutdown.
	// Events still in the channel after this elapses are dropped.
	FlushTimeout time.Duration
}

// DefaultWorkerConfig is the production default.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		BufferSize:   10000,
		Workers:      2,
		FlushTimeout: 5 * time.Second,
	}
}

// Worker enqueues Events from the chat hot path and persists them to
// request_logs. Construction is cheap; Start spawns the consumer
// goroutines, Stop drains and waits.
type Worker struct {
	pool    *storage.Pool
	pricing *PricingCache
	metrics *observability.Metrics // nil-safe
	logger  *zap.Logger
	cfg     WorkerConfig

	ch      chan Event
	dropped atomic.Int64
	written atomic.Int64

	// Partition cache: month "2026-04" → already ensured. Avoids
	// CREATE TABLE IF NOT EXISTS DDL on every event.
	partMu sync.Mutex
	partOK map[string]struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// NewWorker constructs (but does not start) a worker. pricing may be
// nil — events from un-priced models will record zero cost. pool must
// be non-nil; the worker is the only consumer of request_logs writes.
// metrics is optional; nil disables Prometheus instrumentation.
func NewWorker(pool *storage.Pool, pricing *PricingCache, metrics *observability.Metrics, cfg WorkerConfig, logger *zap.Logger) (*Worker, error) {
	if pool == nil {
		return nil, fmt.Errorf("billing: worker requires a non-nil pool")
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = DefaultWorkerConfig().BufferSize
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkerConfig().Workers
	}
	if cfg.FlushTimeout <= 0 {
		cfg.FlushTimeout = DefaultWorkerConfig().FlushTimeout
	}
	return &Worker{
		pool:    pool,
		pricing: pricing,
		metrics: metrics,
		logger:  logger,
		cfg:     cfg,
		ch:      make(chan Event, cfg.BufferSize),
		partOK:  make(map[string]struct{}),
	}, nil
}

// Start spawns cfg.Workers consumer goroutines. Idempotent: subsequent
// calls are no-ops. Returns immediately; goroutines run until Stop.
func (w *Worker) Start(ctx context.Context) {
	w.startOnce.Do(func() {
		for i := 0; i < w.cfg.Workers; i++ {
			w.wg.Add(1)
			go w.run(ctx)
		}
		if w.logger != nil {
			w.logger.Info("billing worker started",
				zap.Int("buffer", w.cfg.BufferSize),
				zap.Int("workers", w.cfg.Workers))
		}
	})
}

// Enqueue submits an event for asynchronous persistence. Returns true
// when accepted, false when the buffer was full (drop counted via
// DroppedCount). Non-blocking by design; the chat hot path must never
// be paused on billing.
func (w *Worker) Enqueue(e Event) bool {
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now()
	}
	select {
	case w.ch <- e:
		return true
	default:
		w.dropped.Add(1)
		w.metrics.IncBillingDropped()
		return false
	}
}

// DroppedCount is the count of events that hit a full buffer since
// Worker construction. Surfaced as a Prometheus gauge in Step 8.
func (w *Worker) DroppedCount() int64 { return w.dropped.Load() }

// WrittenCount is the count of events successfully INSERTed.
func (w *Worker) WrittenCount() int64 { return w.written.Load() }

// Stop signals the consumers to exit, then waits up to FlushTimeout
// for the channel to drain. Events still queued after the deadline
// are dropped (logged at warn). Idempotent.
func (w *Worker) Stop(ctx context.Context) {
	w.stopOnce.Do(func() {
		close(w.ch)

		done := make(chan struct{})
		go func() {
			w.wg.Wait()
			close(done)
		}()

		flushCtx, cancel := context.WithTimeout(ctx, w.cfg.FlushTimeout)
		defer cancel()

		select {
		case <-done:
			if w.logger != nil {
				w.logger.Info("billing worker stopped cleanly",
					zap.Int64("written", w.written.Load()),
					zap.Int64("dropped", w.dropped.Load()))
			}
		case <-flushCtx.Done():
			if w.logger != nil {
				w.logger.Warn("billing worker stop timed out; some events may be lost",
					zap.Int64("written", w.written.Load()),
					zap.Int64("dropped", w.dropped.Load()),
					zap.Int("queued_remaining", len(w.ch)))
			}
		}
	})
}

// run is the consumer loop. Exits when the channel closes or ctx is
// canceled. Each event INSERTs one row (and a partition CREATE if
// needed). Errors are logged but never re-enqueued — request_logs is
// best-effort, not a guaranteed ledger.
func (w *Worker) run(ctx context.Context) {
	defer w.wg.Done()
	for ev := range w.ch {
		if ctx.Err() != nil {
			// Caller is shutting down. Stop draining.
			return
		}
		w.persist(ctx, ev)
	}
}

func (w *Worker) persist(ctx context.Context, ev Event) {
	if err := w.ensurePartition(ctx, ev.StartedAt); err != nil {
		if w.logger != nil {
			w.logger.Warn("ensure partition failed",
				zap.Error(err),
				zap.Time("started_at", ev.StartedAt))
		}
		// Try the insert anyway — postgres will reject with a clear
		// error, which we log below. We don't want a transient DDL
		// race to drop the event silently.
	}

	totalTokens := ev.PromptTokens + ev.CompletionTokens

	var inputMicro, outputMicro, totalMicro int64
	currency := "USD"
	if w.pricing != nil {
		if rate, ok := w.pricing.Lookup(ev.Model); ok {
			inputMicro = int64(ev.PromptTokens) * rate.InputPer1kMicro / 1000
			outputMicro = int64(ev.CompletionTokens) * rate.OutputPer1kMicro / 1000
			totalMicro = inputMicro + outputMicro
			if rate.Currency != "" {
				currency = rate.Currency
			}
		}
	}

	const q = `
		INSERT INTO request_logs (
			started_at, request_id, api_key_id, provider, model,
			prompt_tokens, completion_tokens, total_tokens,
			latency_ms, status, streamed,
			input_micro, output_micro, total_micro, currency, error_code
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8,
			$9, $10, $11,
			$12, $13, $14, $15, $16
		)`

	insertCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err := w.pool.Exec(insertCtx, q,
		ev.StartedAt, ev.RequestID, nullableString(ev.APIKeyID),
		ev.Provider, ev.Model,
		ev.PromptTokens, ev.CompletionTokens, totalTokens,
		ev.LatencyMs, ev.Status, ev.Streamed,
		inputMicro, outputMicro, totalMicro, currency,
		nullableString(ev.ErrorCode),
	); err != nil {
		if w.logger != nil {
			w.logger.Warn("insert request_log failed",
				zap.Error(err),
				zap.String("request_id", ev.RequestID),
				zap.String("model", ev.Model))
		}
		return
	}
	w.written.Add(1)
	w.metrics.IncBillingWritten()
	w.metrics.AddCost(ev.Provider, ev.Model, ev.APIKeyID, totalMicro)
}

// ensurePartition creates the month-partition for ts if it doesn't yet
// exist. Cached per-month so the DDL only fires the first time a worker
// sees a new month — subsequent events in the same month skip the
// check entirely.
func (w *Worker) ensurePartition(ctx context.Context, ts time.Time) error {
	key := ts.UTC().Format("2006-01")
	w.partMu.Lock()
	_, ok := w.partOK[key]
	w.partMu.Unlock()
	if ok {
		return nil
	}

	year, month, _ := ts.UTC().Date()
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	// Partition naming: request_logs_YYYYMM. Postgres identifiers max
	// 63 chars; this is well within bounds.
	partName := fmt.Sprintf("request_logs_%s", start.Format("200601"))
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF request_logs
		   FOR VALUES FROM ('%s') TO ('%s')`,
		partName,
		start.Format(time.RFC3339),
		end.Format(time.RFC3339),
	)
	if _, err := w.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create partition %s: %w", partName, err)
	}

	w.partMu.Lock()
	w.partOK[key] = struct{}{}
	w.partMu.Unlock()
	return nil
}

// nullableString turns "" into nil so NULL is stored rather than empty
// string — keeps queries like `WHERE api_key_id IS NULL` meaningful.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
