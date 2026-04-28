package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/storage"
)

// runPricing dispatches `xbctl pricing <list|set|delete>`. The
// sub-subcommand model mirrors `xbctl migrate <up|down|version>`.
//
// All paths write directly to model_pricing via PricingCache.Set/Delete,
// so a running gateway picks up changes on its next periodic reload
// (default 30 min) — operators wanting instant convergence should
// either restart the gateway or call the admin HTTP API.
func runPricing(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		printPricingUsage(stdout)
		return fmt.Errorf("pricing subcommand required")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runPricingList(rest, stdout)
	case "set":
		return runPricingSet(rest, stdout)
	case "delete":
		return runPricingDelete(rest, stdout)
	case "-h", "--help", "help":
		printPricingUsage(stdout)
		return nil
	default:
		printPricingUsage(stdout)
		return fmt.Errorf("unknown pricing subcommand %q", sub)
	}
}

func printPricingUsage(w io.Writer) {
	fmt.Fprint(w, `xbctl pricing — manage model_pricing

Usage:
  xbctl pricing list
  xbctl pricing set <model> <input_per_1k> <output_per_1k> [--currency USD]
  xbctl pricing delete <model>

Rates are quoted per 1000 tokens in major currency units (e.g. 0.005 = $0.005/1k).
Storage is integer micro-units to avoid float drift.
`)
}

func runPricingList(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("pricing list", flag.ContinueOnError)
	fs.SetOutput(stdout)
	in := registerDSNFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dsn, err := resolveDSN(in)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn, MaxConns: 2})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	cache := billing.NewPricingCache(pool, zap.NewNop())
	if err := cache.Reload(ctx); err != nil {
		return fmt.Errorf("reload pricing: %w", err)
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tCURRENCY\tINPUT/1K\tOUTPUT/1K\tUPDATED")
	rates := cache.All()
	sortRatesByModel(rates)
	for _, r := range rates {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Model, r.Currency,
			fmtMicro(r.InputPer1kMicro),
			fmtMicro(r.OutputPer1kMicro),
			r.UpdatedAt.UTC().Format("2006-01-02 15:04"),
		)
	}
	return tw.Flush()
}

func runPricingSet(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("pricing set", flag.ContinueOnError)
	fs.SetOutput(stdout)
	in := registerDSNFlags(fs)
	currency := fs.String("currency", "USD", "ISO currency code (default USD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 3 {
		return fmt.Errorf("usage: xbctl pricing set <model> <input_per_1k> <output_per_1k>")
	}
	model := rest[0]
	inRate, err := parseRate(rest[1])
	if err != nil {
		return fmt.Errorf("input_per_1k: %w", err)
	}
	outRate, err := parseRate(rest[2])
	if err != nil {
		return fmt.Errorf("output_per_1k: %w", err)
	}

	dsn, err := resolveDSN(in)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn, MaxConns: 2})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	cache := billing.NewPricingCache(pool, zap.NewNop())
	if err := cache.Set(ctx, billing.Rate{
		Model: model, Currency: *currency,
		InputPer1kMicro:  inRate,
		OutputPer1kMicro: outRate,
	}); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}

	fmt.Fprintf(stdout, "Set pricing for %q: input=%s output=%s %s/1k\n",
		model, fmtMicro(inRate), fmtMicro(outRate), *currency)
	return nil
}

func runPricingDelete(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("pricing delete", flag.ContinueOnError)
	fs.SetOutput(stdout)
	in := registerDSNFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: xbctl pricing delete <model>")
	}
	model := rest[0]

	dsn, err := resolveDSN(in)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn, MaxConns: 2})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	cache := billing.NewPricingCache(pool, zap.NewNop())
	ok, err := cache.Delete(ctx, model)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if !ok {
		fmt.Fprintf(stdout, "No pricing entry for %q (already absent).\n", model)
		return nil
	}
	fmt.Fprintf(stdout, "Deleted pricing for %q.\n", model)
	return nil
}

// parseRate accepts a decimal like "0.005" and converts to micro-units.
// Invalid / negative values are rejected; the database CHECK constraint
// would catch them too, but a CLI error is friendlier.
func parseRate(s string) (int64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid rate %q: %w", s, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("rate must be >= 0, got %v", v)
	}
	// Float → int64 micro-units. We round half-up so 0.0001 doesn't
	// silently become 0 due to FP representation drift.
	return int64(v*1_000_000 + 0.5), nil
}

// fmtMicro formats micro-units back to a human-readable major-unit
// decimal (e.g. 5000 → "0.005000"). Always 6 decimals to make column
// alignment consistent in `pricing list`.
func fmtMicro(m int64) string {
	major := float64(m) / 1_000_000
	return strconv.FormatFloat(major, 'f', 6, 64)
}

// sortRatesByModel uses a tiny insertion sort to avoid pulling in
// "sort" for a single call site — fine for the typical pricing-table
// size (dozens of rows). If the table ever grows past a few hundred
// rows, swap to sort.Slice.
func sortRatesByModel(rs []billing.Rate) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j-1].Model > rs[j].Model; j-- {
			rs[j-1], rs[j] = rs[j], rs[j-1]
		}
	}
}

