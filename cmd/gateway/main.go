// Command gateway is the entry point for the X-BEACON LLM inference gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/audit"
	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/config"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/server"
	"github.com/An-idd/x-beacon/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWithCtx(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// parseArgs is split out from run so tests can drive the flow without
// touching the global flag.CommandLine.
func parseArgs(args []string) (configPath string, showVersion bool, err error) {
	fs := flag.NewFlagSet("x-beacon", flag.ContinueOnError)
	fs.StringVar(&configPath, "config", "configs/config.yaml", "path to config file")
	fs.BoolVar(&showVersion, "version", false, "print version and exit")
	err = fs.Parse(args)
	return
}

// runWithCtx is the testable core of main(): it owns the full startup +
// shutdown lifecycle but receives ctx and args injected by the caller, so
// tests can cancel mid-run or supply temporary configs.
func runWithCtx(ctx context.Context, args []string, stdout *os.File) error {
	configPath, showVersion, err := parseArgs(args)
	if err != nil {
		return err
	}
	if showVersion {
		fmt.Fprintln(stdout, version.Banner())
		return nil
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, err := observability.NewLogger(observability.LogConfig{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
	})
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	metricsReg := observability.NewMetricsRegistry()
	metrics, err := observability.NewMetrics(metricsReg)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}

	reg, err := loadRegistry(cfg.ProvidersFile, logger)
	if err != nil {
		return fmt.Errorf("init provider registry: %w", err)
	}

	pool, err := loadPool(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("init storage pool: %w", err)
	}
	if pool != nil {
		defer pool.Close()
	}

	authn, err := loadAuth(ctx, pool, logger)
	if err != nil {
		return fmt.Errorf("init auth: %w", err)
	}

	// Wrap Authenticator with the Redis cache when both Redis is up AND a
	// real authenticator was constructed. Either component being absent
	// degrades cleanly to "no cache" — the gateway still works, just
	// hotter on the DB.
	rdb := loadRedis(ctx, cfg, logger)
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
		if authn != nil && cfg.Auth.Cache.PositiveTTL > 0 {
			authn = auth.NewCached(authn, rdb,
				cfg.Auth.Cache.PositiveTTL, cfg.Auth.Cache.NegativeTTL, logger)
			logger.Info("auth cache enabled",
				zap.Duration("positive_ttl", cfg.Auth.Cache.PositiveTTL),
				zap.Duration("negative_ttl", cfg.Auth.Cache.NegativeTTL))
		}
	}

	tp, shutdownTracing, err := observability.NewTracerProvider(ctx, observability.TracingConfig{
		Enabled:     cfg.Observability.Tracing.Enabled,
		Endpoint:    cfg.Observability.Tracing.Endpoint,
		ServiceName: cfg.Observability.Tracing.ServiceName,
		SampleRatio: cfg.Observability.Tracing.SampleRatio,
		// ServiceName carries through to the Server's Tracing middleware too,
		// keeping span attribution consistent.
	})
	if err != nil {
		return fmt.Errorf("init tracer: %w", err)
	}
	otel.SetTracerProvider(tp)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(sctx); err != nil {
			logger.Warn("tracer shutdown error", zap.Error(err))
		}
	}()

	checkers := buildReadinessCheckers(pool, rdb)

	rateLimiter, err := buildRateLimiter(cfg, rdb, logger)
	if err != nil {
		return fmt.Errorf("init rate limiter: %w", err)
	}

	rtr := buildRouter(cfg, reg, metrics, logger)

	tk, err := buildTokenizer(logger)
	if err != nil {
		return fmt.Errorf("init tokenizer: %w", err)
	}

	billingWorker, pricingCache, err := buildBilling(ctx, cfg, pool, metrics, logger)
	if err != nil {
		return fmt.Errorf("init billing: %w", err)
	}
	if billingWorker != nil {
		defer billingWorker.Stop(context.Background())
	}

	exactCache, cacheTTL := buildExactCache(cfg, rdb, logger)
	semanticCache := buildSemanticCache(cfg, rdb, metrics, logger)
	classifier := buildClassifier(cfg, tk, logger)
	compressor := buildCompressor(cfg, tk, logger)

	var keystore *auth.Keystore
	var auditRecorder audit.Recorder = audit.Nop()
	if pool != nil {
		keystore = auth.NewKeystore(pool, rdb, logger)
		auditRecorder = audit.NewPostgres(pool)
	}

	srv, err := server.New(server.Deps{
		Logger:            logger,
		Registry:          reg,
		Router:            rtr,
		Tokenizer:         tk,
		Billing:           billingWorker,
		Pricing:           pricingCache,
		Keystore:          keystore,
		StoragePool:       pool,
		Stats:             observability.NewStatsCollector(metricsReg, metrics.StartedAt()),
		CacheStats:        observability.NewCacheStatsCollector(metricsReg, metrics.StartedAt()),
		RateLimitRules:    cfg.RateLimits,
		Audit:             auditRecorder,
		Metrics:           metrics,
		Authn:             authn,
		RateLimiter:       rateLimiter,
		Cache:             exactCache,
		CacheTTL:          cacheTTL,
		Semantic:          semanticCache,
		Classifier:        classifier,
		Compressor:        compressor,
		MetricsReg:        metricsReg,
		MetricsEnabled:    cfg.Observability.Metrics.Enabled,
		MetricsPath:       cfg.Observability.Metrics.Path,
		AdminCORSOrigins:  cfg.Server.AdminCORSOrigins,
		ServiceName:       cfg.Observability.Tracing.ServiceName,
		ReadinessCheckers: checkers,
	})
	if err != nil {
		return fmt.Errorf("init server: %w", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadTimeout:       cfg.Server.ReadTimeout,
		ReadHeaderTimeout: cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server starting",
			zap.String("addr", cfg.Server.Addr),
			zap.String("version", version.Version),
			zap.String("commit", version.Commit),
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	sctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(sctx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}
	logger.Info("server stopped cleanly")
	return nil
}
