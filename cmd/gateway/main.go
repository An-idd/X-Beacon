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

	"github.com/An-idd/x-beacon/internal/config"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/server"
)

// Populated at build time via -ldflags (see Makefile).
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
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
		fmt.Fprintf(stdout, "x-beacon %s (commit %s, built %s)\n", version, commit, buildTime)
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

	reg, err := loadRegistry(cfg.ProvidersFile, logger)
	if err != nil {
		return fmt.Errorf("init provider registry: %w", err)
	}

	authn, err := loadAuth(cfg.AuthFile, logger)
	if err != nil {
		return fmt.Errorf("init auth: %w", err)
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

	srv, err := server.New(server.Deps{
		Logger:         logger,
		Registry:       reg,
		Authn:          authn,
		MetricsReg:     metricsReg,
		MetricsEnabled: cfg.Observability.Metrics.Enabled,
		MetricsPath:    cfg.Observability.Metrics.Path,
		ServiceName:    cfg.Observability.Tracing.ServiceName,
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
			zap.String("version", version),
			zap.String("commit", commit),
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
