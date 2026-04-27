// Package version exposes the binary's build metadata. Values are
// injected at link time via -ldflags from the Makefile; the defaults
// here are what unit tests and `go run` see.
//
// Why a separate package: gateway, xbctl, and the three provider
// adapters all want the build version (for --version output, log
// fields, and outbound User-Agent). Keeping the values in cmd/gateway
// would force every provider to import a main package, which Go
// disallows. pkg/version sits at the leaf so anyone can import it.
package version

// These vars are settable via:
//
//	-ldflags '-X github.com/An-idd/x-beacon/pkg/version.Version=...'
//
// They are NOT constants because -X works only on string vars.
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

// UserAgent is the value the gateway sends to upstream LLM APIs as the
// User-Agent header. Format is the minimal `x-beacon/<version>` so
// upstream log greps stay simple.
func UserAgent() string {
	return "x-beacon/" + Version
}

// Banner is the long-form build line used by --version and startup logs.
func Banner() string {
	return "x-beacon " + Version + " (commit " + Commit + ", built " + BuildTime + ")"
}
