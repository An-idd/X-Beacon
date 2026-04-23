# 架构设计文档

> 本文档面向希望深入理解 X-BEACON 内部设计的开发者、贡献者，以及关心"为什么这么设计"的读者。
> 
> 如果你只是想快速使用，请看 [README.md](../README.md)。

## 目录

- [1. 设计目标](#1-设计目标)
- [2. 整体架构](#2-整体架构)
- [3. 核心模块设计](#3-核心模块设计)
- [4. 关键设计决策](#4-关键设计决策)
- [5. 数据流与请求链路](#5-数据流与请求链路)
- [6. 数据模型](#6-数据模型)
- [7. 可观测性设计](#7-可观测性设计)
- [8. 部署架构](#8-部署架构)
- [9. 与同类项目对比](#9-与同类项目对比)
- [10. 未来演进](#10-未来演进)

---

## 1. 设计目标

### 1.1 功能目标

X-BEACON 的核心定位是 **LLM 推理网关（LLM Inference Gateway）**，在业务应用和 LLM 服务提供商之间提供一个统一的中间层，提供以下能力：

| 能力 | 说明 | 优先级 |
|------|------|--------|
| 统一 API | 用一套 OpenAI 兼容的 API 调用所有模型 | P0 |
| 成本控制 | 缓存、token 统计、智能路由 | P0 |
| 可靠性 | 重试、降级、熔断、限流 | P0 |
| 可观测性 | 指标、日志、追踪 | P0 |
| 多租户 | 按用户/组织维度隔离和计费 | P1 |
| 合规审计 | 请求/响应日志持久化、审计追踪 | P2 |

### 1.2 非功能目标

- **性能**：单机 5000+ QPS，空请求 P99 <5ms
- **资源占用**：常驻内存 <500MB（不含缓存数据）
- **可用性**：无状态设计，支持水平扩展，单实例故障不影响整体
- **可维护性**：模块化设计，核心逻辑测试覆盖率 >80%

### 1.3 非目标（Non-Goals）

明确声明**不做**的事情，避免范围蔓延：

- ❌ **不做模型训练/微调**：只是推理层的网关
- ❌ **不做 Agent 编排**：专注于 LLM 调用层，不涉及工具调用链路管理
- ❌ **不做向量数据库**：语义缓存的向量存储复用 Redis 或外接向量库
- ❌ **不做完整的 LLMOps 平台**：聚焦网关职责，不承担 prompt 管理、数据标注等功能

---

## 2. 整体架构

### 2.1 系统上下文图

```
┌─────────────────────────────────────────────────────────────┐
│                      Client Applications                    │
│     (Web Apps, Mobile Apps, Backend Services, CLI Tools)    │
└───────────────────────────┬─────────────────────────────────┘
                            │ HTTPS (OpenAI Compatible API)
                            ▼
┌─────────────────────────────────────────────────────────────┐
│                       X-BEACON Gateway                        │
│  ┌─────────────────────────────────────────────────────┐    │
│  │                 HTTP Server (chi)                   │    │
│  │                                                     │    │
│  │   Auth  →  RateLimit  →  Cache  →  Router          │    │
│  │                                        │           │    │
│  │                                        ▼           │    │
│  │                              Provider Adapters     │    │
│  │                                        │           │    │
│  │                                        ▼           │    │
│  │                              Billing & Logging     │    │
│  └─────────────────────────────────────────────────────┘    │
│                                                             │
│  ┌─────────┐    ┌──────────┐    ┌────────────────┐         │
│  │  Redis  │    │Postgres  │    │  Prometheus    │         │
│  │(cache,  │    │(metadata,│    │  (metrics)     │         │
│  │ limit)  │    │ billing) │    │                │         │
│  └─────────┘    └──────────┘    └────────────────┘         │
└───────────────────────────┬─────────────────────────────────┘
                            │
                            ▼
           ┌──────────────────────────────────┐
           │        LLM Providers             │
           │  OpenAI | Anthropic | DeepSeek   │
           │  Qwen   | Doubao    | ...        │
           └──────────────────────────────────┘
```

### 2.2 核心组件

| 组件 | 职责 | 实现要点 |
|------|------|----------|
| HTTP Server | 接收请求、返回响应 | 基于标准库 + chi，支持 SSE |
| Middleware Chain | 横切关注点（认证、限流等） | 责任链模式，每个中间件独立可测 |
| Provider Adapter | 对接各 LLM 提供商 | 统一接口，协议转换 |
| Cache Layer | 精确缓存 + 语义缓存 | Redis + embedding 向量 |
| Router | 选择合适的 provider / model | 规则引擎 + 健康检查 |
| Billing | Token 计数、成本计算 | 异步写入，不阻塞主路径 |
| Observability | 指标、日志、追踪 | Prometheus + OTel + Zap |

### 2.3 部署拓扑

X-BEACON 采用**无状态设计**，所有状态存储在外部的 Redis 和 PostgreSQL 中。这意味着：

- 可以任意水平扩展实例数
- 任一实例故障不影响其他实例
- 滚动升级零停机

```
    Load Balancer
         │
    ┌────┼────┬────┐
    ▼    ▼    ▼    ▼
   H1   H2   H3   Hn     ← 无状态 X-BEACON 实例
    │    │    │    │
    └────┴────┴────┘
         │
    ┌────┴────┐
    ▼         ▼
  Redis   Postgres   ← 共享状态
  Cluster Primary
           │
           ▼
        Replica
```

---

## 3. 核心模块设计

### 3.1 Provider 抽象层

这是整个项目最核心的抽象。所有 LLM 提供商通过统一的 `Provider` 接口暴露能力：

```go
type Provider interface {
    // Name 返回 provider 的唯一标识
    Name() string
    
    // ChatCompletion 非流式对话接口
    ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    
    // ChatCompletionStream 流式对话接口
    // 返回的 channel 会被 provider 实现者关闭
    ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan *ChatStreamChunk, error)
    
    // Embeddings 向量化接口
    Embeddings(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error)
    
    // SupportedModels 返回支持的模型列表
    SupportedModels() []ModelInfo
    
    // HealthCheck 健康检查，用于故障转移
    HealthCheck(ctx context.Context) error
}
```

**关键设计点**：

1. **使用 OpenAI 格式作为"通用格式"**：请求和响应的数据结构与 OpenAI API 保持一致，其他 provider 在内部做格式转换。这样的好处是：用户的 OpenAI SDK 代码可以零改动迁移过来。

2. **流式响应统一用 channel**：不同 provider 的流式协议各不相同（OpenAI 用 SSE，Anthropic 有自己的事件格式），但对上层而言都是 `<-chan *ChatStreamChunk`，完全屏蔽协议差异。

3. **Provider 实现重试和错误归一化**：每个 provider 根据自己的特性处理重试策略（比如 OpenAI 的 429 和 Anthropic 的 overloaded_error 行为不同），然后把错误归一化为统一的错误类型。

### 3.2 中间件链

请求处理流程采用经典的中间件责任链模式：

```
Request
   │
   ▼
[Recovery]        ← 捕获 panic，防止进程崩溃
   │
   ▼
[Logging]         ← 记录请求 ID、开始时间
   │
   ▼
[Tracing]         ← 开启 trace span
   │
   ▼
[Auth]            ← API key 验证
   │
   ▼
[RateLimit]       ← 检查限流
   │
   ▼
[Cache Read]      ← 查询缓存
   │
   ├── Hit ──────────────────────────────┐
   │                                     │
   ▼ Miss                                │
[Router]          ← 选择 provider        │
   │                                     │
   ▼                                     │
[Provider Call]   ← 调用 LLM 服务        │
   │                                     │
   ▼                                     │
[Cache Write]     ← 写入缓存             │
   │                                     │
   ▼                                     │
[Billing]         ← 异步记录成本         │
   │                                     │
   ▼                                     │
Response ◀────────────────────────────────┘
```

**设计要点**：
- 每个中间件遵循标准签名 `func(next http.Handler) http.Handler`，完全独立、可测试
- Cache Hit 时直接短路返回，不走后续流程
- Billing 通过 channel 异步处理，不阻塞响应

### 3.3 语义缓存（差异化核心）

语义缓存是本项目相较于同类方案的核心差异化。设计思路：

```
┌──────────────────────────────────────────────────────────┐
│                   Semantic Cache Flow                    │
│                                                          │
│  1. 请求到达                                             │
│       │                                                  │
│       ▼                                                  │
│  2. 计算 prompt 的 embedding 向量                        │
│       │                                                  │
│       ▼                                                  │
│  3. 在向量索引中查找 top-K 相似历史请求                   │
│       │                                                  │
│       ▼                                                  │
│  4. 判断最相似请求的相似度是否 >= 阈值（默认 0.95）       │
│       │                                                  │
│       ├── Yes ──▶ 返回缓存响应                           │
│       │                                                  │
│       └── No ──▶ 调用 LLM，存储请求 + 响应 + 向量        │
└──────────────────────────────────────────────────────────┘
```

**关键决策**：

- **使用 HNSW 算法做 ANN 查询**：在精度和速度间取得好的平衡。初期可以直接用 Redis 的 RediSearch 模块，未来可以替换为专门的向量库（Qdrant、Milvus）
- **Embedding 模型可配置**：默认使用 `text-embedding-3-small`（便宜），用户可切换
- **相似度阈值可调**：默认 0.95 保证精确性，用户可以根据场景调整
- **防止缓存污染**：对响应做基础质量检查（非空、非错误响应）后才入缓存

### 3.4 限流设计

支持多层级、多维度的限流规则：

```yaml
# 限流规则示例
rate_limits:
  # 全局限流
  - name: global
    algorithm: token_bucket
    rate: 10000/s
    burst: 15000
  
  # 按 API key 限流
  - name: per_key
    algorithm: sliding_window
    window: 1m
    limit: 60  # 每分钟 60 次
    key_by: api_key
  
  # 按模型限流（贵的模型严格限制）
  - name: per_model_gpt4
    algorithm: token_bucket
    rate: 100/s
    conditions:
      model: "gpt-4*"
  
  # 按用户 + 模型组合限流
  - name: per_user_per_model
    algorithm: sliding_window
    window: 1h
    limit: 1000
    key_by: [user_id, model]
```

**算法选择**：
- **令牌桶**：适合应对突发流量（允许短时间 burst）
- **滑动窗口**：适合精确控制（严格按时间窗口计数）

**分布式实现**：基于 Redis + Lua 脚本保证原子性，参考 Envoy 的实现。

### 3.5 成本归因

```
┌─────────────────────────────────────────────────────────┐
│  请求完成 → 异步计费流水线                              │
│                                                         │
│  1. 从响应中提取 usage 字段（prompt_tokens, etc）       │
│  2. 若 provider 不返回 usage，用本地 tokenizer 计算     │
│  3. 根据模型价目表计算成本                              │
│  4. 写入 billing_records 表                             │
│  5. 更新 Redis 中的实时成本计数（用于限额检查）         │
└─────────────────────────────────────────────────────────┘
```

**关键点**：
- Tokenizer 基于 tiktoken，支持所有主流模型
- 计费流水线通过 buffered channel 异步处理，1 个 worker goroutine 就能处理 10k QPS
- 价目表配置化，方便模型调价

---

## 4. 关键设计决策

本节记录项目中的关键技术决策和 trade-off 分析。格式参考 [ADR (Architecture Decision Records)](https://adr.github.io/)。

### ADR-001：为什么选择 Go 而非 Python？

**背景**：LLM 生态大部分是 Python（LiteLLM、LangChain 等），但我们选择了 Go。

**决策**：使用 Go 作为主要实现语言。

**理由**：
1. **性能**：网关场景对延迟敏感，Go 的并发模型（goroutine）比 Python 的 asyncio 性能更好，且无 GIL 瓶颈
2. **部署简单**：单个静态二进制文件，Docker 镜像可做到 <20MB
3. **内存占用**：同等功能下 Go 实现的内存占用通常是 Python 的 1/3 到 1/5
4. **生态**：Go 在云原生网关领域有成熟生态（Envoy、Traefik 都有借鉴价值）

**代价**：
- LLM 相关库（tokenizer、embedding）需要自己封装或调用 HTTP 服务
- 社区贡献者门槛略高

### ADR-002：为什么不用 Gin/Echo 等框架？

**背景**：Go 有许多成熟的 Web 框架。

**决策**：使用标准库 `net/http` + `chi` 路由。

**理由**：
1. **性能**：`net/http` 在 Go 1.22+ 性能已经非常优秀，框架的额外抽象反而增加开销
2. **可控性**：网关场景需要精细控制请求生命周期（特别是流式响应），框架封装反而碍事
3. **依赖精简**：`chi` 零外部依赖，整个项目的依赖树更干净
4. **长期维护**：标准库稳定性远高于第三方框架

### ADR-003：为什么选 OpenAI 格式作为统一格式？

**决策**：所有 provider 的请求/响应在内部转换为 OpenAI 格式。

**理由**：
1. **事实标准**：OpenAI API 格式已成为 LLM 领域的事实标准，大量工具链基于此设计
2. **迁移无痛**：用户的现有代码可以零改动接入 X-BEACON
3. **生态友好**：所有 OpenAI SDK 可以直接使用

**代价**：
- 某些 provider 的特殊能力（如 Anthropic 的 system prompt 结构）需要做兼容处理
- OpenAI 格式迭代时需要跟进

### ADR-004：为什么不用 ORM？

**决策**：直接使用 `pgx` 写 SQL。

**理由**：
1. **性能**：ORM 的反射和自动生成 SQL 开销不可忽视
2. **可控性**：网关场景的 SQL 很简单（主要是 insert 和按 key 查询），不需要 ORM 的复杂特性
3. **可读性**：对于熟悉 SQL 的开发者，直接 SQL 反而更易读

**代价**：
- 表结构变更时需要手动维护 SQL 语句
- 通过 `sqlc` 或代码生成可以部分缓解

### ADR-005：缓存应该放在 Gateway 还是独立服务？

**背景**：语义缓存涉及 embedding 计算，逻辑较重。

**决策**：内置在 Gateway，但保留拆分为独立服务的接口扩展点。

**理由**：
- **当前**：内置实现复杂度更低，延迟更短
- **未来**：当缓存规模扩大（需要独立扩容）或需要跨 Gateway 共享缓存时，通过接口切换到远程缓存服务

### ADR-006：流式响应的实现方式

**背景**：LLM 调用普遍需要流式返回，流式处理是网关的核心复杂度来源。

**决策**：用 Go channel 作为内部统一抽象，HTTP 层用 SSE 输出。

**理由**：
1. Channel 是 Go 的原生并发原语，语义清晰
2. SSE 是业界主流的流式协议，兼容性好
3. 易于扩展到其他协议（如 WebSocket）

**挑战**：
- 流中间断开的错误处理复杂
- 需要特别小心 goroutine 泄漏

---

## 5. 数据流与请求链路

### 5.1 典型请求的完整链路

以一个带缓存未命中的请求为例：

```
Time  Event
───── ─────────────────────────────────────────────────
 0ms  Client 发送 HTTP POST /v1/chat/completions
 1ms  HTTP Server 接收请求
 2ms  Recovery 中间件启动
 2ms  Logging 中间件记录请求 ID
 3ms  Tracing 中间件开启 root span
 3ms  Auth 中间件从 header 提取 API key，查询 Redis 验证
 5ms  RateLimit 中间件执行限流检查（Redis Lua 脚本）
 7ms  Cache Read：计算 prompt hash，查询 Redis
 8ms  Cache Miss
 8ms  Router 选择 provider（根据 model 字段）
 9ms  Provider Adapter 转换请求格式
 10ms HTTP Client 发起到 OpenAI 的请求
 ...  (等待 OpenAI 响应)
 2010ms OpenAI 返回响应
 2011ms Provider Adapter 转换响应格式
 2012ms Cache Write：异步写入缓存（不阻塞）
 2012ms Billing：异步记录成本（不阻塞）
 2013ms 响应写回 Client
 2013ms 关闭 trace span，记录指标
```

### 5.2 流式请求的特殊处理

```
Client                X-BEACON              Provider
  │                     │                     │
  │──POST /v1/chat/────▶│                     │
  │    completions      │                     │
  │ (stream: true)      │                     │
  │                     │                     │
  │                     │──POST /v1/chat/────▶│
  │                     │    completions      │
  │                     │ (stream: true)      │
  │                     │                     │
  │                     │◀──SSE chunk 1───────│
  │◀──SSE chunk 1───────│                     │
  │                     │◀──SSE chunk 2───────│
  │◀──SSE chunk 2───────│                     │
  │                     │    ...              │
  │                     │◀──SSE [DONE]────────│
  │◀──SSE [DONE]────────│                     │
  │                     │                     │
  │                     │ (聚合 chunks → 计算  │
  │                     │  token → 异步写缓存) │
```

**关键点**：
- X-BEACON 边收边转发，不等整个响应完成
- 流结束后（收到 `[DONE]`）触发缓存写入和计费
- 中途断开时不写入缓存（避免缓存不完整响应）

---

## 6. 数据模型

### 6.1 主要数据表

```sql
-- API Key 表
CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash TEXT NOT NULL UNIQUE,   -- 存储 key 的 hash，不存明文
    name TEXT NOT NULL,
    user_id UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    enabled BOOLEAN NOT NULL DEFAULT true,
    rate_limit_config JSONB,
    metadata JSONB
);

-- 请求日志表（按月分区）
CREATE TABLE request_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    api_key_id UUID NOT NULL,
    request_id TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    status TEXT NOT NULL,
    prompt_tokens INTEGER,
    completion_tokens INTEGER,
    total_tokens INTEGER,
    cost_usd NUMERIC(10, 6),
    duration_ms INTEGER,
    cache_hit BOOLEAN DEFAULT false,
    cache_type TEXT,                 -- 'exact' / 'semantic' / null
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
) PARTITION BY RANGE (created_at);

CREATE INDEX idx_request_logs_key_time ON request_logs(api_key_id, created_at DESC);
CREATE INDEX idx_request_logs_model ON request_logs(model, created_at DESC);

-- Provider 配置表
CREATE TABLE providers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    enabled BOOLEAN NOT NULL DEFAULT true,
    config JSONB NOT NULL,           -- endpoint, api_key, timeout 等
    priority INTEGER DEFAULT 0,     -- 用于故障转移
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 模型价目表
CREATE TABLE model_pricing (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    prompt_price_per_1k NUMERIC(10, 6) NOT NULL,
    completion_price_per_1k NUMERIC(10, 6) NOT NULL,
    effective_from TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(provider, model, effective_from)
);
```

### 6.2 Redis 数据结构

```
# 限流计数（滑动窗口用 ZSET，令牌桶用 STRING）
ratelimit:sliding:{key_hash}:{window}    → ZSET
ratelimit:bucket:{key_hash}              → STRING (JSON)

# 精确缓存
cache:exact:{prompt_hash}                → STRING (JSON response)

# 语义缓存的向量索引（RediSearch）
cache:semantic                           → FT.CREATE idx

# 实时成本（用于限额）
cost:{api_key_id}:{window}               → STRING (number)

# Provider 健康状态
health:{provider}                         → STRING (ok/failing)
```

---

## 7. 可观测性设计

### 7.1 指标（Metrics）

核心指标（Prometheus 格式）：

```
# 请求量
gateway_requests_total{provider, model, status}

# 延迟分布
gateway_request_duration_seconds{provider, model} [histogram]
gateway_provider_duration_seconds{provider, model} [histogram]  # 仅 provider 调用时间

# Token 用量
gateway_tokens_total{provider, model, type}   # type: prompt/completion

# 成本
gateway_cost_usd_total{provider, model, api_key}

# 缓存
gateway_cache_hits_total{type}                # type: exact/semantic
gateway_cache_misses_total

# 限流
gateway_ratelimit_rejected_total{rule}

# 错误
gateway_errors_total{provider, error_type}

# 资源
process_resident_memory_bytes
go_goroutines
```

### 7.2 日志（Logs）

结构化日志，每条包含：
- `request_id`：全局唯一，用于追踪
- `api_key_id`
- `provider`、`model`
- `status`、`duration_ms`
- `prompt_tokens`、`completion_tokens`
- `cache_hit`

**敏感信息处理**：
- 不记录完整 prompt 和响应（只记录 hash 和 token 数量）
- API key 只记录 hash 前缀

### 7.3 追踪（Tracing）

使用 OpenTelemetry，关键 span：
- `http.request`：HTTP 处理完整链路
- `middleware.auth`、`middleware.ratelimit`、`middleware.cache`
- `provider.{name}.call`：调用 provider 的 HTTP 请求
- `billing.record`：计费写入

每个 span 包含 `request_id`，便于关联日志和 trace。

---

## 8. 部署架构

### 8.1 容器化

```dockerfile
# 多阶段构建，最终镜像 <20MB
FROM golang:1.22-alpine AS builder
# ... 编译
FROM alpine:3.19
COPY --from=builder /app/x-beacon /usr/local/bin/
ENTRYPOINT ["x-beacon"]
```

### 8.2 Kubernetes 部署

```yaml
# 核心 Deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: x-beacon
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: x-beacon
        image: x-beacon:latest
        resources:
          requests:
            cpu: 500m
            memory: 256Mi
          limits:
            cpu: 2000m
            memory: 1Gi
        readinessProbe:
          httpGet:
            path: /healthz
            port: 8080
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8080

---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: x-beacon
spec:
  minReplicas: 3
  maxReplicas: 30
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 70
```

### 8.3 高可用考量

- **无状态**：X-BEACON 实例可任意扩缩容
- **Redis HA**：Sentinel 或 Redis Cluster
- **Postgres HA**：主从 + 自动故障转移
- **多可用区**：实例和依赖分布在至少 2 个 AZ

---

## 9. 与同类项目对比

| 维度 | X-BEACON | LiteLLM | OneAPI | Portkey |
|------|--------|---------|--------|---------|
| 语言 | Go | Python | Go | TypeScript |
| 性能（P99） | <5ms | ~80ms | ~15ms | ~10ms |
| 内存占用 | 低 | 高 | 中 | 中 |
| 语义缓存 | ✅ | ❌ | ❌ | ✅（商业版） |
| 可观测性 | 完整 | 基础 | 基础 | 完整 |
| 开源协议 | Apache 2.0 | MIT | MIT | 部分开源 |
| 社区 | 起步中 | 活跃 | 活跃（中文） | 商业主导 |

**X-BEACON 的定位**：在开源、高性能、生产级三个维度之间取得平衡。

---

## 10. 未来演进

### 近期（3-6 个月）
- 完善现有核心功能（缓存、限流、可观测性）
- 增加更多 provider 支持
- 推出 Web 管理面板

### 中期（6-12 个月）
- 多模态支持（图片、音频）
- Function calling 标准化
- Prompt 优化引擎（自动压缩、上下文裁剪）
- 插件系统（允许用户自定义中间件）

### 长期
- 作为更大的 AI Infra 平台的一部分
- 与模型训练/微调服务集成
- 企业级特性（SSO、审计、合规）

---

## 附录

### A. 参考资料
- [OpenAI API Reference](https://platform.openai.com/docs/api-reference)
- [Anthropic API Docs](https://docs.anthropic.com/en/api)
- [Envoy Architecture](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/arch_overview)
- [Design of LLM Gateways (论文/博客)](#)

### B. 术语表
- **LLM**：Large Language Model，大语言模型
- **Provider**：LLM 服务提供商（如 OpenAI、Anthropic）
- **ANN**：Approximate Nearest Neighbor，近似最近邻搜索
- **HNSW**：Hierarchical Navigable Small World，一种 ANN 算法
- **SSE**：Server-Sent Events，服务器推送事件
- **ADR**：Architecture Decision Record，架构决策记录

### C. 变更记录
- 2026-04-23：初版文档
