# X-BEACON 运维手册（Operational Runbook）

> Phase 3 收官时整理。覆盖 cache / smart routing / prompt compression /
> billing 的常见运维动作。每一节都形如「症状 → 排查 → 操作命令」，
> 设计目标是值班工程师**不读源码**也能恢复服务。

---

## 1. 精确缓存（exact cache）

**模块**：`internal/cache/exact.go`，Redis string，key `cache:exact:{sha256}`。

### 1.1 一键关闭（紧急）

发现缓存返回错误响应（污染）时，先关写、再清读：

```bash
# 关闭缓存写入（保留命中读，避免雪崩）
sed -i '' 's/cache:\n  exact:\n    enabled: true/cache:\n  exact:\n    enabled: false/' configs/config.yaml
make restart
```

### 1.2 清空缓存

```bash
# 全量清（会有短时间 latency 抖动，所有请求转发到上游）
redis-cli --scan --pattern 'cache:exact:*' | xargs -L 100 redis-cli DEL

# 单 key 删除（已知污染 hash 时优先用这个）
redis-cli DEL cache:exact:<sha256>
```

### 1.3 重 warm

正常情况下缓存自然 warm。如需主动加热，回放生产日志即可：

```bash
# 假设你有最近 1h 的请求体落盘（admin tooling 待 v0.4 加）
cat /tmp/replay.jsonl | xargs -I{} curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $WARM_KEY" -H "Content-Type: application/json" -d {}
```

---

## 2. 语义缓存（semantic cache）

**模块**：`internal/cache/semantic.go` + RediSearch HNSW。

### 2.1 阈值调整

观察 `gateway_cache_semantic_similarity` histogram 与 `gateway_cache_semantic_threshold`
gauge 的相对位置。如果 P50 命中相似度长期 ≥ 0.97 而阈值是 0.95，可以收紧：

```yaml
cache:
  semantic:
    threshold: 0.97  # was 0.95
```

收紧后下游误命中率下降；放宽则命中率上升、误命中风险上升。**每次只动 0.01–0.02**。

### 2.2 删除 per-model 索引

每个 model 一个 RediSearch 索引（`semanticCacheSelector`）。某个模型上线
新版本想清空只属于它的语义条目：

```bash
# 列出所有索引
redis-cli FT._LIST

# 删某个 model 的索引（不删除 hash 数据，需要单独清）
redis-cli FT.DROPINDEX cache:semantic:idx:gpt-4o-mini DD  # DD=delete docs
```

### 2.3 Embedding 上游故障

`pkg/embedding/openai.go` 的错误归一化到 `provider.ErrUpstream`。
日志会出现 `semantic cache write failed`；查询路径走 fail-open
（miss 当作正常未命中）。无需立即处理，只需观察
`gateway_cache_writes_total{type="semantic"}` 是否归零。

如要强制语义缓存彻底关闭：`cache.semantic.enabled: false` + 重启。

---

## 3. 智能路由（smart routing）

**模块**：`internal/route/rule_classifier.go`。

### 3.1 加规则

编辑 `configs/config.yaml`：

```yaml
routing:
  enabled: true
  rules:
    - name: cheap-translate
      route_to: gpt-4o-mini
      when:
        keywords_any: [translate, 翻译]
        max_tokens: 1000
    # 新增规则：长上下文走 claude
    - name: long-context-to-claude
      route_to: claude-3-5-sonnet
      when:
        min_tokens: 50000
```

规则**首匹配 wins**，所以顺序很重要。重启生效（v0.4 之前没有热加载）。

### 3.2 让某个 API key 不走路由（A/B 控制组）

```bash
./bin/xbctl keygen --scope smart_route:disable --label "ab-control-arm-1"
```

该 key 的请求会跳过 classifier，response 头 `X-X-Beacon-Route-Rule: skip:scope`
确认；指标 `gateway_router_bypass_total{reason="scope"}` 计数递增。

### 3.3 监控规则有没有过度激进

`gateway_router_decision_total{from,to,rule}` 的 `rule` 维度看哪条规则
触发最多。如果某条规则吃了 > 70% 流量，说明它写得太宽：考虑拆分或
加 `keywords_none`。

---

## 3.5 Scope 列表（API key 权限）

API key 的 scope 是字符串元组 `category:value`（JSONB 存在 `api_keys.scopes`）。
`xbctl keygen --scope cat:val` 签发；中间件用 `RequireScope(category, value)` 守路由。

| Scope | 受保护资源 | 谁应该有 |
|-------|-----------|---------|
| `admin:webui` | `/admin/keys/*` / `/admin/logs` / `/admin/stats/*` | WebUI 运维账号 |
| `admin:pricing` | `/admin/pricing/*`（GET/PUT/DELETE） | 财务 / 定价管理员 |
| `smart_route:disable` | 路由层短路（A/B 对照组） | 实验对照组的客户端 key |
| _(empty)_ | 仅 `/v1/*`（OpenAI 兼容 API） | 普通业务 key |

**约定**：
- 格式严格 `^[a-z]+:[a-z_]+$`（admin endpoint 强制校验，CLI 不强制但建议遵守）
- 多 scope：`xbctl keygen --scope admin:webui --scope admin:pricing`
- 想看某 key 的 scope：`xbctl keylist | grep <id>`
- 不存在"super-admin"全权 scope；缺哪个加哪个

**新增 scope 流程**：
1. 在本表新增一行
2. handler 加 `RequireScope("category", "value")` middleware
3. 给运维签新 key（旧 key 不会自动获得新 scope）

---

## 4. Prompt 压缩（context truncation）

**模块**：`internal/prompt/compressor.go`。

### 4.1 紧急关闭

```yaml
prompt:
  compression:
    enabled: false  # 客户端将看到原始上下文 — 可能撞上游 context_length_exceeded
```

### 4.2 调整触发阈值

默认 `trigger_ratio: 0.8`，即 prompt 占模型上下文 80% 时才介入。如果
观察到 `gateway_prompt_compressed_total` 远高于预期：

```yaml
prompt:
  compression:
    trigger_ratio: 0.9   # 更晚介入，丢弃更少历史
```

如果客户端频繁报 context_length_exceeded：

```yaml
prompt:
  compression:
    trigger_ratio: 0.7   # 更早介入，丢弃更多历史
```

### 4.3 添加新模型的 context window

```yaml
prompt:
  compression:
    default_window: 128000
    model_windows:
      gpt-4o-mini:  128000
      claude-3-5-sonnet: 200000
      # 新增
      my-fine-tune: 32768
```

### 4.4 验证压缩生效

```bash
curl -i -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d @huge-prompt.json | grep X-X-Beacon-Prompt-Compressed
# 期待：X-X-Beacon-Prompt-Compressed: 1
```

OTel trace 里搜 span name `prompt.compress`，attributes 含
`prompt.tokens_before/after` 直接看裁剪量。

---

## 5. Billing / Pricing

**模块**：`internal/billing/`，月度分区表 `request_logs`。

### 5.1 修改某个 model 的 pricing

```bash
./bin/xbctl pricing set \
  --model gpt-4o \
  --prompt-rate-per-million 5000000 \
  --completion-rate-per-million 15000000
# 单位是 micro-USD（5_000_000 = $5.00 / 1M tokens）

# 立即重载（避免等 30min 周期）— 调 admin API
curl -X POST http://localhost:8080/admin/pricing/reload \
  -H "Authorization: Bearer $ADMIN_KEY"
```

### 5.2 Worker 队列堆积

观察 `gateway_billing_dropped_total`。> 0 即写入跟不上：

1. 看 DB 慢查询（`pg_stat_statements`）
2. 临时扩容 `billing.worker.workers`（默认 2，可加到 4–8）
3. 长期方案：加索引或换更高 IOPS 的实例

**重要**：drop 是**事件丢失**，不会阻塞请求。但财务对账会缺数据。

### 5.3 月度分区维护

新月份自动懒创建（写入第一条时建分区）。**不需要 cron**。
如要手动预创建（防月初峰值卡顿）：

```sql
-- 提前建下个月的分区
SELECT billing_create_partition_for_date(now() + interval '1 month');
```

---

## 6. 健康检查与 readiness

- `/healthz` — 进程活着即返回 200。**不要**用作 LB 探针，否则 DB 挂掉时仍会派流量。
- `/readyz` — 检查 DB + Redis 连通（1s 整体 deadline）。这个挂上 LB。

预期 latency：`/healthz` < 1ms，`/readyz` < 50ms（含一次 DB Ping + 一次 Redis Ping）。

---

## 7. 应急联系

> v0.4 之前没有正式 oncall，问题汇报到项目仓库 issue。

**版本信息**：每个二进制都有 `--version`：

```bash
./bin/x-beacon --version
# x-beacon v0.3.0 (commit abcdef0, built 2026-04-29)
```
