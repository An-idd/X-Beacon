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

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/config"
	"github.com/An-idd/x-beacon/internal/observability"
)

// Populated at build time via -ldflags (see Makefile).
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  string
		showVersion bool
	)
	flag.StringVar(&configPath, "config", "configs/config.yaml", "path to config file")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("x-beacon %s (commit %s, built %s)\n", version, commit, buildTime)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	tp, shutdownTracing, err := observability.NewTracerProvider(ctx, observability.TracingConfig{
		Enabled:     cfg.Observability.Tracing.Enabled,
		Endpoint:    cfg.Observability.Tracing.Endpoint,
		ServiceName: cfg.Observability.Tracing.ServiceName,
		SampleRatio: cfg.Observability.Tracing.SampleRatio,
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

	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	if cfg.Observability.Metrics.Enabled {
		r.Handle(cfg.Observability.Metrics.Path, promhttp.HandlerFor(metricsReg, promhttp.HandlerOpts{Registry: metricsReg}))
	}

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           r,
		ReadTimeout:       cfg.Server.ReadTimeout,
		ReadHeaderTimeout: cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server starting",
			zap.String("addr", cfg.Server.Addr),
			zap.String("version", version),
			zap.String("commit", commit),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}
	logger.Info("server stopped cleanly")
	return nil
}
