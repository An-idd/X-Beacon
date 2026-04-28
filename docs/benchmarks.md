# X-BEACON benchmarks

Numbers below are **gateway-only overhead**: latency added by X-BEACON
in front of an instant-mock upstream. Real LLM completions take seconds;
these measurements isolate what the gateway adds on top.

---

## Methodology

Tooling: [`tsenart/vegeta`](https://github.com/tsenart/vegeta) for
constant-rate load. Acceptance latencies are taken at **steady state**
after a 5 s warm-up (auth cache + pricing cache become hot).

Driver script: [`scripts/bench.sh`](../scripts/bench.sh). Three or four
endpoints are exercised in order, longest path last:

| # | Path | What it covers |
|---|------|----------------|
| 1 | `/healthz` | Pure routing; lower bound for all middleware |
| 2 | `/readyz` | + DB Ping + Redis Ping per request |
| 3 | `/v1/models` | + Auth (cache-warm) + cache-cold variant for comparison |
| 4 | `/v1/chat/completions` | + Tokenizer + Router + breaker + mock upstream + billing enqueue |

Mock upstream returns a canned 200 with usage=`{1,1,2}`; recorded chunks
are exercised only on the OpenAI-compat handler that the mock targets.

---

## M1 baseline (2026-04-27)

Environment: `will@10.109.8.217` (Mac mini / macOS 26.3 / arm64) ·
vegeta + gateway same host · gateway dev-mode (no DB / no Redis / empty
registry) · commit `41896c3`.

`/healthz`:

| RPS    | P50   | P99       | Errors |
|--------|-------|-----------|--------|
| 200    | 254µs | **528µs** | 0      |
| 500    | 216µs | **420µs** | 0      |
| 1000   | 201µs | **329µs** | 0      |
| 5000   | 42µs  | **108µs** | 0      |
| 10000  | 38µs  | **100µs** | 0      |
| 20000  | 38µs  | **224µs** | 0      |
| 30000  | 41µs  | **366µs** | 0      |
| 50000  | 87µs  | **737µs** | 0      |

`/v1/models` (chi `/v1/*` subrouter + JSON encode, no auth/DB):

| RPS  | P50   | P99       | Errors |
|------|-------|-----------|--------|
| 1000 | 207µs | **358µs** | 0      |
| 5000 | 43µs  | **122µs** | 0      |

**Conclusion**: M1 line "P99 < 10 ms on empty requests" passed with
**> 10× margin**. CLAUDE.md "5000 QPS sustained" target also exceeded;
50k RPS produced zero errors.

## Week 5 ratelimit accuracy (2026-04-27)

Same host. Single `memory_bucket` rule `rate=100/s, burst=100`, global
(no `key_by`).

| Vegeta input | Time | Total | 200    | 429   | Theoretical pass | Deviation |
|--------------|------|-------|--------|-------|------------------|-----------|
| 50 RPS       | 15 s | 750   | 750    | 0     | 750              | 0%        |
| 100 RPS      | 15 s | 1500  | 1500   | 0     | 1500             | 0%        |
| 150 RPS      | 15 s | 2250  | 1599   | 651   | 1600             | **−0.06%** |
| 200 RPS      | 20 s | 4000  | 2099   | 1901  | 2100             | **−0.05%** |

**Conclusion**: M2 line "ratelimit deviation < 5%" passed with 80×+
margin (−0.05% / −0.06% measured).

---

## Week 8 chat-path bench (M2 acceptance, 2026-04-28)

Environment: local Mac mini · macOS 15.6.1 / Darwin 24.6.0 / arm64 ·
Postgres 16 + Redis Stack 7.4 in docker · gateway, vegeta and mock
upstream all on the same host · commit `3586586`.

Setup used (reproducible):

```bash
# 1. Dependencies.
docker compose up -d postgres redis

# 2. Build + migrate.
make build
./bin/xbctl migrate -config configs/config.yaml up

# 3. Mint a key (one-shot).
./bin/xbctl keygen -config configs/config.yaml -name bench
# secret is printed once; export it as GATEWAY_KEY for the bench.

# 4. Mock upstream (returns canned 200 with usage={1,1,2}).
go build -o bin/mockupstream ./scripts/mockupstream
./bin/mockupstream &        # listens on 127.0.0.1:9091

# 5. Point providers.yaml at the mock (single openai-compat provider,
#    endpoint=http://127.0.0.1:9091, model gpt-4o-mini), then:
./bin/x-beacon --config configs/config.yaml &

# 6. Run the bench.
RATE=1000 DURATION=30s GATEWAY_KEY=sk-... \
  MOCK_MODEL=gpt-4o-mini scripts/bench.sh
```

Acceptance: **P99 < 20 ms** on `/v1/chat/completions` at sustained
1000 RPS against an instant mock upstream.

### Results

`/v1/chat/completions` (full hot path: auth → ratelimit → router →
tokenizer → mock provider → billing enqueue):

| RPS  | P50     | P95     | P99       | Max     | Errors | Notes      |
|------|---------|---------|-----------|---------|--------|------------|
| 200  | 1.467ms | 2.761ms | **6.484ms** | 21.0ms | 0/3000 | warm-up    |
| 1000 | 362µs   | 504µs   | **1.213ms** | 65.4ms | 0/30000 | acceptance |
| 5000 | 308µs   | 570µs   | **1.093ms** | 22.5ms | 0/100000 | sustained |

Companion paths at 1000 RPS / 30 s for context:

| Path           | P50     | P95     | P99       |
|----------------|---------|---------|-----------|
| `/healthz`     | (see Week 4 baseline) | | |
| `/readyz`      | 697µs   | 1.08ms  | **2.10ms** |
| `/v1/models` warm | 609µs | 942µs  | **1.79ms** |

Billing pipeline at the end of the run:
`gateway_billing_written_total = 164001`,
`gateway_billing_dropped_total = 0` — async writer kept up with
combined 200 + 1000 + 5000 RPS without a single drop.

**Conclusion**: M2 line "P99 < 20 ms on `/v1/chat/completions` at
1000 RPS" passed with **~16× margin** (1.21 ms measured). The 5000 RPS
sustained line in CLAUDE.md also passed (P99 = 1.09 ms, 0 errors over
100k requests). Hot path is comfortably sub-2 ms; the visible tails
(max 22–65 ms) are GC + connection-establishment, not steady-state
work.

---

## Reproducing on the M1 baseline host

The remote `will@10.109.8.217` retained the Week 4 binaries under
`~/xbeacon-bench/`. To re-run after a code change:

```bash
# Local: cross-compile if needed (binaries already match darwin/arm64).
GOOS=darwin GOARCH=arm64 make build
scp bin/x-beacon bin/xbctl will@10.109.8.217:~/xbeacon-bench/

# Remote: restart + bench.
ssh will@10.109.8.217 'pkill x-beacon; ~/xbeacon-bench/x-beacon --config ~/xbeacon-bench/config.yaml &'
ssh will@10.109.8.217 'echo "GET http://127.0.0.1:8080/healthz" | ~/xbeacon-bench/vegeta attack -rate=10000 -duration=15s | ~/xbeacon-bench/vegeta report'
```
