// Package envexpand resolves the gateway's YAML env-var placeholder syntax
// against the current process environment. Both providers.yaml and
// auth.yaml use it, so the rules live here in one place.
//
// Supported syntax:
//
//	${VAR}            — simple braced expansion
//	${VAR:-default}   — expansion with fallback when VAR is unset/empty
//
// Unbraced $VAR is intentionally NOT expanded, to avoid colliding with
// strings that legitimately contain "$" (e.g. cost-pattern placeholders).
package envexpand

import (
	"os"
	"regexp"
)

// envPattern requires VAR to start with a letter or underscore and contain
// only [A-Za-z0-9_], matching POSIX shell variable rules.
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// Expand replaces supported placeholders in s using the current process
// environment. Unknown-and-no-default placeholders expand to the empty
// string (matching shell semantics). Callers should explicitly validate
// required fields after expansion.
func Expand(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := envPattern.FindStringSubmatch(match)
		name, def := groups[1], groups[2]
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		return def
	})
}
