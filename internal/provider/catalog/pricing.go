package catalog

import (
	"fmt"
	"strings"

	"github.com/An-idd/x-beacon/internal/provider"
)

// FormatPricing converts internal micro-unit prices to the wire-format
// ModelPricing struct (strings, USD per 1k tokens). Both halves must be
// > 0 for the result to be non-nil: if either is zero we treat it as
// "price not published" and return nil so the handler omits the field
// (better than showing a misleading "$0.00").
//
// The string form is fixed-point (no scientific notation, trailing zeros
// trimmed but at least one digit after the decimal) so clients can parse
// it with naive decimal libraries.
func FormatPricing(promptPer1kMicro, completionPer1kMicro int64, currency string) *provider.ModelPricing {
	if promptPer1kMicro <= 0 || completionPer1kMicro <= 0 {
		return nil
	}
	if currency == "" {
		currency = "USD"
	}
	return &provider.ModelPricing{
		Prompt:     microToUSDString(promptPer1kMicro),
		Completion: microToUSDString(completionPer1kMicro),
		Currency:   currency,
		Unit:       "1K_tokens",
	}
}

// microToUSDString formats an int64 micro-USD value as a decimal string,
// e.g. 140 → "0.00014", 10000 → "0.01", 2500000 → "2.5". Trailing zeros
// after the decimal are trimmed but never the whole fractional part
// (so "1.00" stays "1.0" not "1").
//
// Implemented manually to avoid float64 precision loss: at $0.00014 / 1k
// tokens the 6th decimal matters and strconv.FormatFloat(... 'f') can
// emit "0.00013999999".
func microToUSDString(micro int64) string {
	if micro < 0 {
		return "-" + microToUSDString(-micro)
	}
	whole := micro / 1_000_000
	frac := micro % 1_000_000
	if frac == 0 {
		return fmt.Sprintf("%d.0", whole)
	}
	// Zero-pad fractional to 6 digits, then trim trailing zeros (keeping
	// at least one digit after the dot).
	fracStr := fmt.Sprintf("%06d", frac)
	fracStr = strings.TrimRight(fracStr, "0")
	if fracStr == "" {
		fracStr = "0"
	}
	return fmt.Sprintf("%d.%s", whole, fracStr)
}
