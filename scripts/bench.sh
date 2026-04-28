#!/usr/bin/env bash
# bench.sh — measure gateway-only overhead with vegeta against a mocked
# upstream. Used for the "P99 < 10ms on empty requests" M1 acceptance
# target.
#
# Prereqs:
#   - vegeta installed (brew install vegeta || go install github.com/tsenart/vegeta@latest)
#   - postgres + redis up (`make docker-up`)
#   - migrations applied (`./bin/xbctl migrate up`)
#   - one API key in the table; export GATEWAY_KEY=sk-...
#
# Usage:
#   scripts/bench.sh                           # default: 200 RPS, 30s
#   RATE=500 DURATION=60s scripts/bench.sh

set -euo pipefail

RATE="${RATE:-200}"
DURATION="${DURATION:-30s}"
TARGET_HOST="${TARGET_HOST:-http://127.0.0.1:8080}"
GATEWAY_KEY="${GATEWAY_KEY:?set GATEWAY_KEY to a valid bearer token (xbctl keygen)}"

echo "Bench config:"
echo "  rate:     $RATE rps"
echo "  duration: $DURATION"
echo "  target:   $TARGET_HOST"
echo

# /healthz: pure routing latency, no DB / Redis / provider hop.
# Use this number as the gateway-only floor for P99 budget.
echo "## /healthz (no auth, no DB, no provider)"
echo "GET $TARGET_HOST/healthz" | vegeta attack \
    -rate="$RATE" -duration="$DURATION" -keepalive=true \
  | vegeta report -type=text

echo

# /readyz: hits DB + Redis (Ping). Realistic floor for "auth path warm".
echo "## /readyz (DB + Redis ping every request)"
echo "GET $TARGET_HOST/readyz" | vegeta attack \
    -rate="$RATE" -duration="$DURATION" -keepalive=true \
  | vegeta report -type=text

echo

# /v1/models: full middleware chain incl. Auth (cache-warm path).
# Run twice so the cache is hot for the second run; report the second.
echo "## /v1/models (auth cache cold)"
TARGETS=$(mktemp)
trap 'rm -f "$TARGETS"' EXIT
{
  printf 'GET %s/v1/models\nAuthorization: Bearer %s\n' "$TARGET_HOST" "$GATEWAY_KEY"
} > "$TARGETS"

vegeta attack -targets="$TARGETS" -rate="$RATE" -duration="5s" -keepalive=true \
  >/dev/null

echo "## /v1/models (auth cache warm)"
vegeta attack -targets="$TARGETS" -rate="$RATE" -duration="$DURATION" -keepalive=true \
  | vegeta report -type=text

echo

# /v1/chat/completions: full hot path — Auth + RateLimit + Router +
# tokenizer + provider call (mock) + billing enqueue. Measures the
# realistic "what does an API consumer actually feel" latency.
#
# Requires the gateway to be configured with a mock-OpenAI upstream.
# Set MOCK_MODEL to a model that resolves to that upstream.
MOCK_MODEL="${MOCK_MODEL:-gpt-4o-mini}"
echo "## /v1/chat/completions (full hot path, mock upstream, model=$MOCK_MODEL)"
CHAT_BODY="${CHAT_BODY:-$(printf '{"model":"%s","messages":[{"role":"user","content":"ping"}]}' "$MOCK_MODEL")}"
CHAT_TARGETS=$(mktemp)
CHAT_BODY_FILE=$(mktemp)
trap 'rm -f "$TARGETS" "$CHAT_TARGETS" "$CHAT_BODY_FILE"' EXIT
printf '%s' "$CHAT_BODY" > "$CHAT_BODY_FILE"
# Use @<file> body directive so vegeta reads the JSON body from disk
# (the inline-body trick via @- only works when piping the targets file
# through vegeta on stdin, which we aren't here).
{
  printf 'POST %s/v1/chat/completions\nAuthorization: Bearer %s\nContent-Type: application/json\n@%s\n' \
    "$TARGET_HOST" "$GATEWAY_KEY" "$CHAT_BODY_FILE"
} > "$CHAT_TARGETS"

# Warm-up to populate caches (auth / pricing) so the recorded run is
# representative of steady-state.
vegeta attack -targets="$CHAT_TARGETS" -rate="$RATE" -duration="5s" -keepalive=true \
  >/dev/null

vegeta attack -targets="$CHAT_TARGETS" -rate="$RATE" -duration="$DURATION" -keepalive=true \
  | vegeta report -type=text

echo
echo "Acceptance lines:"
echo "  M1 (Phase 1): P99 < 10ms on cache-warm /v1/models                       — Week 4"
echo "  M2 (Phase 2): P99 < 20ms on /v1/chat/completions (auth+billing+mock up) — Week 8"
