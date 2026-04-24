package registry

import (
	"os"
	"regexp"
)

// envPattern matches:
//
//	${VAR}            — simple braced expansion
//	${VAR:-default}   — expansion with fallback when VAR is unset/empty
//
// Unbraced `$VAR` is intentionally unsupported to avoid ambiguity with
// strings that legitimately contain "$" (e.g., cost pattern placeholders).
// VAR must be uppercase alphanumeric + underscore, starting with letter or "_".
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv replaces supported placeholders in s using the current process
// environment. Unknown-and-no-default placeholders expand to the empty
// string (matching shell semantics). Callers should explicitly validate
// required fields after expansion.
func expandEnv(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := envPattern.FindStringSubmatch(match)
		name, def := groups[1], groups[2]
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		return def
	})
}
