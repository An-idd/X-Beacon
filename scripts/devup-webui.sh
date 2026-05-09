#!/usr/bin/env bash
# devup-webui.sh — bring the gateway + dependencies up locally, sign an
# admin key, and seed enough traffic that the WebUI Dashboard / Logs
# pages have something to render.
#
# Idempotent against rerun: docker volumes survive, migrations are no-op
# when up to date, the admin key gets a unix-suffixed label so re-runs
# don't collide. Old keys stay around — `xbctl keyrevoke` to clean.
#
# Usage:
#   scripts/devup-webui.sh           # start everything, print admin key + URLs
#   scripts/devup-webui.sh stop      # tear down processes (docker stays up)
#
# After running, the WebUI repo can `npm run dev` and proxy to :8080
# without further config.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# --- config --------------------------------------------------------

DSN="${DSN:-postgres://xbeacon:xbeacon@127.0.0.1:5432/xbeacon?sslmode=disable}"
GATEWAY_ADDR="${GATEWAY_ADDR:-127.0.0.1:8080}"
MOCK_ADDR="${MOCK_ADDR:-127.0.0.1:9091}"  # matches mockupstream default + bench.sh providers.yaml
TRAFFIC_OK="${TRAFFIC_OK:-50}"     # successful chat completions to seed
TRAFFIC_ERR="${TRAFFIC_ERR:-5}"    # bad-auth requests to seed errors_4xx
LOG_DIR="/tmp/xbeacon-devup"
GATEWAY_LOG="$LOG_DIR/gateway.log"
MOCK_LOG="$LOG_DIR/mockupstream.log"
GATEWAY_PIDFILE="$LOG_DIR/gateway.pid"
MOCK_PIDFILE="$LOG_DIR/mockupstream.pid"

mkdir -p "$LOG_DIR"

# --- helpers -------------------------------------------------------

log()  { printf "\033[1;34m[devup]\033[0m %s\n" "$*"; }
warn() { printf "\033[1;33m[devup]\033[0m %s\n" "$*"; }
die()  { printf "\033[1;31m[devup]\033[0m %s\n" "$*" >&2; exit 1; }

# wait_port host port label timeout — block until TCP port accepts.
wait_port() {
  local host="$1" port="$2" label="$3" timeout="${4:-30}"
  local n=0
  while ! nc -z "$host" "$port" 2>/dev/null; do
    n=$((n+1))
    if [ "$n" -ge "$timeout" ]; then
      die "$label not reachable on $host:$port after ${timeout}s (check $LOG_DIR for logs)"
    fi
    sleep 1
  done
}

# kill_pidfile — terminate the process tracked in $1 if any.
kill_pidfile() {
  local f="$1" name="$2"
  [ -f "$f" ] || return 0
  local pid
  pid="$(cat "$f" 2>/dev/null || true)"
  if [ -n "${pid:-}" ] && kill -0 "$pid" 2>/dev/null; then
    log "stopping $name (pid $pid)"
    kill "$pid" 2>/dev/null || true
    sleep 1
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$f"
}

# --- subcommand: stop ----------------------------------------------

if [ "${1:-up}" = "stop" ]; then
  kill_pidfile "$GATEWAY_PIDFILE" "gateway"
  kill_pidfile "$MOCK_PIDFILE" "mockupstream"
  log "processes stopped. docker compose left running — \`make docker-down\` to clean."
  exit 0
fi

# --- preflight -----------------------------------------------------

command -v go >/dev/null || die "go not found in PATH"
command -v docker >/dev/null || die "docker not found in PATH"
command -v nc >/dev/null || die "nc (netcat) not found in PATH"
command -v curl >/dev/null || die "curl not found in PATH"

# --- 1. docker dependencies ----------------------------------------

log "starting postgres + redis-stack via docker compose"
make docker-up >/dev/null

log "waiting for postgres on 5432"
wait_port 127.0.0.1 5432 postgres 30
log "waiting for redis on 6379"
wait_port 127.0.0.1 6379 redis 30

# --- 2. build binaries (skip if up to date is handled by make) -----

log "building gateway + xbctl"
make build >/dev/null

# --- 3. migrations -------------------------------------------------

log "applying migrations"
./bin/xbctl migrate -dsn "$DSN" up >/dev/null || die "migrate up failed (check DSN)"

# --- 4a. config.yaml bootstrap -------------------------------------

if [ ! -f configs/config.yaml ]; then
  log "bootstrapping configs/config.yaml from example"
  cp configs/config.example.yaml configs/config.yaml
fi

# --- 4b. providers.yaml pointing at mock upstream ------------------

if [ ! -f configs/providers.yaml ]; then
  log "writing configs/providers.yaml (mock upstream)"
  cat > configs/providers.yaml <<EOF
providers:
  - name: mockupstream
    type: openai
    endpoint: http://${MOCK_ADDR}
    api_key: sk-mock
    models:
      exact: ["gpt-4o-mini", "gpt-4o"]
EOF
else
  warn "configs/providers.yaml exists — leaving as-is. delete it if you want the mock-upstream template."
fi

# --- 5. start mock upstream ----------------------------------------
#
# Liveness probe is the *port*, not the PID. PIDs get reused on darwin
# so a stale PID file can claim "already running" while the real
# process died — caught the user out once already.

if nc -z "${MOCK_ADDR%:*}" "${MOCK_ADDR#*:}" 2>/dev/null; then
  log "mockupstream already listening on $MOCK_ADDR"
else
  rm -f "$MOCK_PIDFILE"  # clear stale PID, if any
  log "starting mockupstream on $MOCK_ADDR"
  MOCK_ADDR="$MOCK_ADDR" nohup go run ./scripts/mockupstream >"$MOCK_LOG" 2>&1 &
  echo $! >"$MOCK_PIDFILE"
  wait_port "${MOCK_ADDR%:*}" "${MOCK_ADDR#*:}" mockupstream 15
fi

# --- 6. start gateway ----------------------------------------------

if nc -z "${GATEWAY_ADDR%:*}" "${GATEWAY_ADDR#*:}" 2>/dev/null; then
  # If a previous run latched the circuit breaker open, it'll stay
  # that way until 30s timeout. Restart by default — it's free, and
  # the alternative is mysteriously-failing seed traffic.
  log "gateway already listening on $GATEWAY_ADDR — restarting to clear any latched breaker state"
  kill_pidfile "$GATEWAY_PIDFILE" "gateway"
  # Fallback: kill anything still on the port even without a tracked pidfile.
  if existing="$(lsof -ti tcp:"${GATEWAY_ADDR#*:}" 2>/dev/null)"; then
    [ -n "$existing" ] && kill "$existing" 2>/dev/null && sleep 1 || true
  fi
fi
rm -f "$GATEWAY_PIDFILE"
log "starting gateway on $GATEWAY_ADDR (logs → $GATEWAY_LOG)"
nohup ./bin/x-beacon -config configs/config.yaml >"$GATEWAY_LOG" 2>&1 &
echo $! >"$GATEWAY_PIDFILE"
wait_port "${GATEWAY_ADDR%:*}" "${GATEWAY_ADDR#*:}" gateway 15

# Belt-and-braces: liveness probe so failures surface here, not in the
# WebUI five minutes later.
if ! curl -fsS "http://${GATEWAY_ADDR}/healthz" >/dev/null; then
  die "gateway /healthz failed — see $GATEWAY_LOG"
fi

# --- 7. sign an admin key ------------------------------------------

KEY_LABEL="webui-dev-$(date +%s)"
log "signing admin key with label '$KEY_LABEL'"

# capture the secret line: format is "  secret: sk-xb-...".
KEYGEN_OUT="$(./bin/xbctl keygen \
  -dsn "$DSN" \
  -name "$KEY_LABEL" \
  -scope admin:webui \
  -scope admin:pricing 2>&1)"
ADMIN_KEY="$(printf "%s\n" "$KEYGEN_OUT" | awk '/secret:/ {print $2}')"
[ -n "$ADMIN_KEY" ] || die "could not parse secret from keygen output:\n$KEYGEN_OUT"

# --- 8. seed traffic -----------------------------------------------

log "seeding $TRAFFIC_OK successful + $TRAFFIC_ERR failed requests"

# Probe one request first so we fail loudly if the upstream chain is
# broken (wrong port in providers.yaml, mockupstream crashed, etc.)
# instead of silently looping 50 errors.
probe_status="$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "http://${GATEWAY_ADDR}/v1/chat/completions" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"probe"}]}')"
if [ "$probe_status" != "200" ]; then
  warn "seed probe returned $probe_status — upstream chain looks broken."
  warn "  Check: configs/providers.yaml endpoint matches MOCK_ADDR ($MOCK_ADDR)"
  warn "  Check: tail $GATEWAY_LOG for circuit-breaker / connection errors"
  warn "  Skipping bulk seed; admin key is still valid for the WebUI."
else
  ok=1  # probe counts
  for _ in $(seq 2 "$TRAFFIC_OK"); do
    curl -fsS -X POST "http://${GATEWAY_ADDR}/v1/chat/completions" \
      -H "Authorization: Bearer $ADMIN_KEY" \
      -H "Content-Type: application/json" \
      -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
      -o /dev/null && ok=$((ok+1)) || true
  done
  log "seeded $ok / $TRAFFIC_OK successful requests"
fi
for _ in $(seq 1 "$TRAFFIC_ERR"); do
  curl -fsS -X POST "http://${GATEWAY_ADDR}/v1/chat/completions" \
    -H "Authorization: Bearer sk-not-a-real-key" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"x"}]}' \
    -o /dev/null || true
done

# --- 9. summary ----------------------------------------------------

cat <<EOF

╭──────────────────────────────────────────────────────────────╮
│  X-BEACON dev stack is up                                    │
╰──────────────────────────────────────────────────────────────╯

  Gateway:       http://${GATEWAY_ADDR}
  Mock upstream: http://${MOCK_ADDR}
  Postgres:      ${DSN}

  Admin key (admin:webui + admin:pricing):
    ${ADMIN_KEY}

  Quick checks:
    curl http://${GATEWAY_ADDR}/healthz
    curl -H "Authorization: Bearer ${ADMIN_KEY}" \\
         http://${GATEWAY_ADDR}/admin/stats/summary

  Logs:
    tail -f ${GATEWAY_LOG}
    tail -f ${MOCK_LOG}

  Stop processes (docker stays):
    scripts/devup-webui.sh stop

  Frontend:
    cd /Users/kk/AI_Project/X-Beacon-Web && npm run dev
    open http://localhost:5173 — paste the admin key above

EOF
