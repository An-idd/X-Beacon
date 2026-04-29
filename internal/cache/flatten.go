package cache

import (
	"strings"

	"github.com/An-idd/x-beacon/internal/provider"
)

// FlattenForEmbedding converts a ChatRequest into the single string we
// hand to the embedding model. The Week 10 strategy is "system + last
// user message" — a deliberate middle ground between recall and noise:
//
//   - Including system message keeps cache scope per persona (a "you
//     are a python tutor" assistant doesn't share keys with "you are a
//     legal advisor").
//   - Last user message captures intent without dragging in stale
//     conversation history that would balloon the embedding text and
//     drift the vector away from the immediate question.
//   - Assistant turns and tool messages are intentionally skipped:
//     they're effects, not causes, and including them makes "same
//     question, different answer history" miss the cache.
//
// Returns "" when there's nothing usable to embed (no user turn, all
// content empty). Callers MUST skip the embed call on "" — calling
// the upstream embedder with empty input wastes a billable round-trip.
//
// Concatenation uses "\n\n" between segments. We do not lowercase or
// strip punctuation: similarity scoring tolerates surface variation
// far better than aggressive normalization, and stripping risks
// collapsing semantically distinct prompts (e.g. "stop." vs "stop").
func FlattenForEmbedding(req *provider.ChatRequest) string {
	if req == nil {
		return ""
	}

	systems := make([]string, 0, 2)
	var lastUser string
	for _, m := range req.Messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		switch m.Role {
		case "system":
			systems = append(systems, content)
		case "user":
			lastUser = content // overwrite — only the most recent matters
		}
		// assistant / tool / function: skip (see doc above).
	}

	if lastUser == "" {
		return ""
	}
	if len(systems) == 0 {
		return lastUser
	}
	return strings.Join(systems, "\n\n") + "\n\n" + lastUser
}
