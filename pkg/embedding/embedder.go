// Package embedding wraps the embedding-vector APIs that semantic
// caching (Week 10) and any future similarity-based feature depend on.
//
// Embedder is intentionally vendor-neutral and chat-agnostic: input is
// a slice of plain strings, output is a slice of float32 vectors with
// fixed Dimensions(). The decision of "what text to embed" — last user
// message? all turns concatenated? system + user? — lives in the
// caller (internal/cache flatten function in Week 10) so it can be
// tuned and tested independently.
//
// Vendors live in subpackages or sibling files. The OpenAI
// implementation is the only one Week 9 ships.
package embedding

import (
	"context"
	"errors"
)

// Embedder produces fixed-dimension vectors from text. Implementations
// must be safe for concurrent use.
type Embedder interface {
	// Embed returns one vector per input string, in input order. The
	// returned slice has the same length as texts. An empty input
	// returns ErrEmptyInput.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions reports the vector length the implementation produces.
	// Stable across all calls; consumers index this when creating the
	// HNSW index.
	Dimensions() int

	// Model is the upstream model identifier (e.g.
	// "text-embedding-3-small"). Surfaced as a label so a dimension
	// change can be detected at startup.
	Model() string
}

// ErrEmptyInput is returned by Embed when texts is nil or empty. Caller
// is expected to filter before invoking — embedding-rate-limited
// upstreams charge per call regardless of whether input is meaningful.
var ErrEmptyInput = errors.New("embedding: empty input")
