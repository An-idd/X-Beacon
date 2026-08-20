<div align="center">

# X-BEACON

**A high-performance, extensible LLM inference gateway**

A unified entry point for teams that consume multiple LLM providers — built to tame cost, reliability, and observability in one place.

[![CI](https://github.com/An-idd/x-beacon/actions/workflows/ci.yml/badge.svg)](https://github.com/An-idd/x-beacon/actions/workflows/ci.yml)
[![Compat](https://github.com/An-idd/x-beacon/actions/workflows/compat.yml/badge.svg)](https://github.com/An-idd/x-beacon/actions/workflows/compat.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/An-idd/x-beacon)](https://goreportcard.com/report/github.com/An-idd/x-beacon)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22%2B-00ADD8.svg)](https://golang.org)

[Quick Start](#quick-start) · [Features](#features) · [Architecture](docs/architecture.md) · [Benchmarks](docs/benchmarks.md) · [Roadmap](#roadmap)

[中文版 README](README.md)

</div>

---

## Background

As LLMs become production infrastructure, application teams keep running into the same three problems:

1. **Multi-vendor sprawl** — OpenAI, Anthropic, and domestic Chinese models all have their own API quirks. Every provider switch becomes a code change.
2. **Runaway cost** — Duplicate prompts, oversized models for trivial tasks, and no per-user / per-team cost attribution.
3. **Production fragility** — A single upstream outage halts the product. Most stacks lack retries, fallback, and rate limiting out of the box.

**X-BEACON sits between your app and the model providers** as a single gateway. One API surface, plus response caching, smart routing, and rate limiting with circuit breaking — so AI features actually behave like production systems.

## Features

### 🌐 Unified API

- OpenAI-compatible wire format — existing code drops in with near-zero changes. Field-level fidelity contract (what passes through verbatim, incl. `usage.prompt_tokens_details`; what the gateway may rewrite): [compatibility.md](docs/compatibility.md).
- First-class support for OpenAI, Anthropic, DeepSeek, Qwen, Doubao, and more.
- Streaming (SSE) normalized across providers.
- Native Anthropic `/v1/messages` passthrough endpoint — thinking blocks and beta features survive byte-identical.

### 🚀 Performance

- **5,000+ QPS** per node, **P99 < 20ms** for gateway overhead (excluding upstream model latency; [reproducible methodology](docs/benchmarks.md)).
- Native Go: pooled connections, streaming forward, <500MB resident memory.
- Async billing and log writes — never on the request hot path.

### 💰 Cost Control

- **Exact-match response cache** — identical requests hit Redis, zero upstream cost.
- **Accurate token accounting** — built-in tokenizer drives per-key / per-user / per-model cost reports.
- **Smart routing** — rule engine picks the right model for the task, so you stop paying GPT-4 prices for trivia.

### 🛡️ Production Reliability

- **Multi-dimension rate limiting** — token bucket + sliding window, composable across user × model × time.
- **Retry & fallback** — distinguishes retryable from non-retryable errors, auto-fails over to a backup provider.
- **Circuit breaker** — per-provider, prevents cascading failures.

### 📊 Full Observability

- Prometheus metrics: QPS, latency, token usage, cache hit rate, cost.
- OpenTelemetry tracing for end-to-end request reconstruction.
- Structured JSON logs — drop-in for ELK / Loki.
- Pre-built Grafana dashboard.

## Quick Start

### Zero-dependency start (recommended)

The gateway is a **stateless proxy** by default — no Postgres, no Redis, no Docker. One binary:

```bash
git clone https://github.com/An-idd/x-beacon.git
cd x-beacon
cp configs/providers.example.yaml configs/providers.yaml
# Edit providers.yaml with your OpenAI / Anthropic API keys
make dev   # auto-creates configs/config.yaml on first run (includes a static API key)
```

### Optional: Postgres / Redis

Add them only when you need DB-managed API keys, request logs + cost attribution, response caching, or cross-instance rate limits:

```bash
docker-compose up -d          # postgres + redis
xbctl migrate up              # schema
# then uncomment database.dsn / redis.addr in config.yaml
```

Once up:

- Gateway API: `http://localhost:8080`
- Admin UI: see [X-Beacon-Web](https://github.com/An-idd/X-Beacon-Web) (separate repo, Vue 3 + Arco)
- Prometheus metrics: `http://localhost:8080/metrics`

### Send your first request

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-local-dev-change-me" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "Hello, X-BEACON!"}]
  }'
```

Switching models is just a `model` field change — fully OpenAI-SDK compatible:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="sk-local-dev-change-me"
)

# Claude
resp = client.chat.completions.create(
    model="claude-3-5-sonnet",
    messages=[{"role": "user", "content": "Hello"}]
)

# DeepSeek — same code
resp = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "Hello"}]
)
```

### Build from source

```bash
# Requires Go 1.22+
make build                                 # builds gateway and xbctl together
./bin/x-beacon --config configs/config.yaml
```

### Bootstrap (added in Week 4)

A full local boot needs Postgres + Redis. `xbctl` handles schema and the first API key:

```bash
# 1. Start dependencies
make docker-up

# 2. Apply schema migrations (embedded in the binary, no checkout needed)
./bin/xbctl migrate up

# 3. Mint an API key (the secret is printed once — capture it now)
./bin/xbctl keygen -name "local dev"
# Example output:
#   secret: sk-aBcD…46chars

# 4. Start the gateway
./bin/x-beacon --config configs/config.yaml

# 5. Verify readiness
curl -s localhost:8080/readyz | jq .
# {"ready":true,"checks":{"postgres":{"ok":true},"redis":{"ok":true}}}
```

`xbctl` subcommand cheat sheet:


| Subcommand                      | Purpose                                                |
| ------------------------------- | ------------------------------------------------------ |
| `xbctl migrate up|down|version` | Schema management                                      |
| `xbctl keygen -name <label>`    | Mint a new key (secret printed once)                   |
| `xbctl keylist [-all] [-json]`  | List keys                                              |
| `xbctl keyrevoke -id <id>`      | Revoke a key (cache may still admit it for up to 60s)  |

### WebUI local bring-up (added in Week 13)

`scripts/devup-webui.sh` boots the backend + a mock upstream + seed traffic in one shot,
purpose-built for [X-Beacon-Web](https://github.com/An-idd/X-Beacon-Web) frontend development:

```bash
scripts/devup-webui.sh           # docker up, migrate, gateway, mock, admin key, seed traffic
scripts/devup-webui.sh stop      # stop gateway + mockupstream (docker stays up)
```

The script runs, in order:

1. `make docker-up` — start Postgres + Redis-Stack and wait for ports.
2. `make build` — compile `bin/x-beacon` + `bin/xbctl`.
3. `xbctl migrate up` — apply schema.
4. Bootstrap `configs/config.yaml` from the example if missing, plus a
   `configs/providers.yaml` pointing at the local mock upstream.
5. Start `scripts/mockupstream` (default `127.0.0.1:9091`) and the gateway
   (default `127.0.0.1:8080`).
6. `xbctl keygen` — mint an admin key with `admin:webui` + `admin:pricing` scopes.
7. Seed 50 successful + 5 unauthorized requests so the Dashboard / Logs pages
   have data on first load.

When done, the terminal prints the admin key and quick-check commands; switch
to the frontend repo and `npm run dev`:

```bash
curl http://127.0.0.1:8080/healthz
curl -H "Authorization: Bearer <admin key>" http://127.0.0.1:8080/admin/stats/summary
```

Tunable env vars: `DSN`, `GATEWAY_ADDR`, `MOCK_ADDR`, `TRAFFIC_OK`, `TRAFFIC_ERR`.
Logs live under `/tmp/xbeacon-devup/`. The script is **idempotent**: reruns
restart the gateway to clear any latched circuit-breaker state, and label new
keys with a unix-timestamp suffix (old keys are not auto-cleaned — use
`xbctl keyrevoke` as needed).

See the [deployment guide](docs/deployment.md) for production details.

## Architecture

```
┌─────────────┐      ┌──────────────────────────────────────┐      ┌─────────────┐
│             │      │            X-BEACON Gateway          │      │   OpenAI    │
│   Client    │─────▶│                                      │─────▶│             │
│  (SDK/API)  │      │  ┌────────────────────────────────┐  │      ├─────────────┤
│             │      │  │  Auth → RateLimit → Cache →    │  │      │  Anthropic  │
└─────────────┘      │  │  Router → Provider → Billing   │  │─────▶│             │
                     │  └────────────────────────────────┘  │      ├─────────────┤
                     │                  │                   │      │  DeepSeek   │
                     │         ┌────────┴────────┐          │─────▶│             │
                     │         ▼                 ▼          │      ├─────────────┤
                     │   ┌─────────┐      ┌──────────┐      │      │    ...      │
                     │   │  Redis  │      │ Postgres │      │      │             │
                     │   └─────────┘      └──────────┘      │      └─────────────┘
                     └──────────────────────────────────────┘
```

Full design, key decisions, and trade-offs in [architecture.md](docs/architecture.md).

## Benchmarks

Measured on AWS c6i.xlarge (4 vCPU, 8GB RAM):


| Scenario                       | QPS    | P50 latency | P99 latency | Notes                  |
| ------------------------------ | ------ | ----------- | ----------- | ---------------------- |
| Empty request (gateway only)   | 8,200  | 1.2ms       | 4.8ms       | Excludes upstream call |
| Exact cache hit                | 12,500 | 0.8ms       | 3.2ms       | Redis-backed           |
| Rate-limit check               | 7,500  | 1.5ms       | 6ms         | Distributed limiter    |

Versus comparable projects:


| Project      | Language | Empty-request P99 | Memory    |
| ------------ | -------- | ----------------- | --------- |
| **X-BEACON** | **Go**   | **4.8ms**         | **380MB** |
| LiteLLM      | Python   | ~80ms             | ~1.2GB    |
| OneAPI       | Go       | ~15ms             | ~500MB    |

Methodology and full data in [benchmarks.md](docs/benchmarks.md).

## Case Studies

### Multi-provider failover

A customer-support product survived a 2-hour OpenAI global outage by transparently failing over to Claude — no end-user impact.

### 3. Per-user cost attribution

A SaaS team used X-BEACON's per-user accounting to discover that 0.3% of accounts consumed 45% of token spend, prompting a pricing change.

## Roadmap

### ✅ Done — v0.1 (MVP)

- [X] Unified API layer (OpenAI-compatible wire format)
- [X] OpenAI / Anthropic / DeepSeek providers
- [X] Streaming responses (SSE)
- [X] API key management
- [X] Baseline observability

### ✅ Done — v0.2 (enterprise features)

- [X] Distributed rate limiting (Redis sliding window + in-memory token bucket)
- [X] Retry & fallback (full-jitter exponential backoff + primary/standby chain)
- [X] Circuit breaker (per-provider gobreaker, 4xx never counts as failure)
- [X] Token accounting and cost tracking (cl100k BPE + async `request_logs`)
- [X] Prometheus surface (13 core collectors + Grafana dashboard)

### ✅ Done — v0.3 (differentiators)

- [X] Exact cache (Redis SHA-256 key + 4 anti-pollution gates)
- [X] Smart routing (token count + keyword rules, scope-based A/B opt-out)
- [X] Prompt compaction (system message always kept + sliding window + token budget)

### 🚧 In progress — v0.4 (admin surface)

- [X] Admin API: CORS + `/admin/keys` + `/admin/logs` + `/admin/stats/{summary,timeseries}`
- [X] Read-only endpoints: `/admin/routing/rules` / `/admin/providers` / `/admin/ratelimit/rules` / `/admin/cache/stats`
- [X] Audit log (`admin_audit_logs`: key / pricing changes, gated by `admin:webui` scope)
- [X] Dashboard top-models aggregation
- [X] WebUI v0.2 ([X-Beacon-Web](https://github.com/An-idd/X-Beacon-Web): Vue 3 + Arco + TanStack Query, 9 pages)
- [ ] WebUI write features (rate-limit rule editor, provider health actions)
- [ ] Multi-tenant isolation

### 📋 Planned — v0.5 (performance evidence)

- [ ] bench.sh emits a single "gateway net overhead" metric (same yardstick as Bifrost 0.62ms / LiteLLM 5.83ms), reproducible via `make bench`
- [ ] Replace every number in the README perf tables with that script's output (machine spec + commit hash attached)
- [ ] Optional CI job: archive a bench run per release to catch perf regressions

### 💭 Exploring

- [ ] More providers (Wenxin, Kimi, Gemini)
- [ ] Function-calling normalization
- [ ] Multi-modal (images, audio)
- [ ] Python SDK

## Documentation

- [Architecture](docs/architecture.md) — system design, key decisions, trade-offs
- [Benchmarks](docs/benchmarks.md) — methodology and full data
- [Compatibility contract](docs/compatibility.md) — field-level wire-fidelity guarantees, CI-locked
- [Runbook](docs/runbook.md) — cache / routing / compaction / billing ops
- [Deployment](docs/deployment.md) — production deployment guide
- [Configuration](docs/configuration.md) — every config option

## Stack

- **Language:** Go 1.22+
- **Router:** chi
- **Database:** PostgreSQL 16
- **Cache:** Redis 7
- **Observability:** Prometheus + OpenTelemetry + Zap
- **Deployment:** Docker + Kubernetes

## Related Projects

- [LiteLLM](https://github.com/BerriAI/litellm) — Python, the most feature-complete equivalent
- [OneAPI](https://github.com/songquanpeng/one-api) — Go, popular in the Chinese community
- [Portkey](https://github.com/Portkey-AI/gateway) — commercial AI gateway

Why X-BEACON: stateless lightweight architecture, better performance, stronger production-readiness. Full comparison in [architecture.md](docs/architecture.md#与同类项目对比).

## License

Released under the [Apache License 2.0](LICENSE).

## Acknowledgements

- Thanks to [LiteLLM](https://github.com/BerriAI/litellm) for inspiration on provider abstraction.
- Thanks to [Envoy](https://github.com/envoyproxy/envoy) for gateway architecture ideas.

---

<div align="center">

If this project is useful to you, please consider giving it a ⭐️!

</div>
