# Grafana provisioning

The dashboard JSON in [`dashboards/x-beacon.json`](dashboards/x-beacon.json)
visualises the metric set from [`internal/observability`](../../internal/observability).
Eight panels cover the three M2 angles (latency / cost / reliability) plus
the Week 6/7 increments:

| # | Panel | What it answers |
|---|-------|-----------------|
| 1 | QPS by status | Are 4xx / 5xx spiking? |
| 2 | Latency P50/P95/P99 by model | Is any model regressing? |
| 3 | Tokens/sec by model + type | Volume mix across providers |
| 4 | Spend / minute (USD) | Burn rate per model |
| 5 | Failover hops + ratelimit rejections | Reliability + abuse signal |
| 6 | Circuit breaker state per provider | At-a-glance upstream health |
| 7 | Billing pipeline (writes vs drops) | Backpressure on `request_logs` |
| 8 | Top API keys by spend (last 1h) | Tenant ops |

## Import (one-shot)

Grafana → Dashboards → New → Import → upload
`dashboards/x-beacon.json`. Pick your existing Prometheus datasource at
the prompt; the dashboard variable `${datasource}` re-binds automatically.

## Provisioning (containerized Grafana)

The `provisioning/` directory matches Grafana's expected layout. Mount
it into a Grafana container together with the dashboards directory:

```yaml
# docker-compose.yml fragment
grafana:
  image: grafana/grafana:11.3.0
  ports: ["3000:3000"]
  volumes:
    - ./deploy/grafana/provisioning:/etc/grafana/provisioning
    - ./deploy/grafana/dashboards:/var/lib/grafana/dashboards
  environment:
    - GF_AUTH_ANONYMOUS_ENABLED=true
    - GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
```

Adjust the Prometheus datasource URL in
[`provisioning/datasources.yaml`](provisioning/datasources.yaml) to point
at your scrape target.

## Cost panels

Cost metrics are stored in **micro-units** (`gateway_cost_micro_total`)
to keep the math integer-safe in Go. Grafana queries divide by
1_000_000 to display USD. If you change `model_pricing.currency`, also
swap the panel unit (`currencyUSD` → other) on the relevant row.

## Adding panels

Each new gateway metric should land here too. Conventions:

- Use `${datasource}` as the variable so the dashboard works against
  any Prometheus instance.
- Counters → `rate(...[1m])` for short-term traffic; `increase(...[1h])`
  for absolute volumes.
- Histograms → `histogram_quantile(0.99, sum by (le, ...) (rate(..._bucket[5m])))`.
- Avoid high-cardinality labels in panel `legendFormat` (e.g. don't
  group by `api_key_id` on a recurring panel — only `topk(...)`).
