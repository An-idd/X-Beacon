<div align="center">

# X-BEACON

**高性能、可扩展的 LLM 推理网关**

为使用多家 LLM 服务的团队提供统一接入层，解决成本、可靠性、可观测性三大痛点。
[![CI](https://github.com/An-idd/x-beacon/actions/workflows/ci.yml/badge.svg)](https://github.com/An-idd/x-beacon/actions/workflows/ci.yml)
[![Compat](https://github.com/An-idd/x-beacon/actions/workflows/compat.yml/badge.svg)](https://github.com/An-idd/x-beacon/actions/workflows/compat.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/An-idd/x-beacon)](https://goreportcard.com/report/github.com/An-idd/x-beacon)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22%2B-00ADD8.svg)](https://golang.org)

[快速开始](#快速开始) · [核心特性](#核心特性) · [架构设计](docs/architecture.md) · [性能基准](docs/benchmarks.md) · [路线图](#路线图)

[English README](README.en.md)

</div>

---

## 背景

随着企业越来越多地使用 LLM 服务，开发团队普遍面临三个问题：

1. **多供应商管理复杂**：OpenAI、Anthropic、国内大模型各有各的 API 规范，每切换一家就要改一次代码
2. **成本失控**：重复的 prompt、无差别地使用最贵的模型、没有成本归因手段
3. **生产可靠性不足**：单一供应商故障直接影响业务，缺少降级、重试、限流等基础能力

**X-BEACON 在应用层和模型层之间提供一个统一的网关**，用一套 API 管理所有 LLM 调用，并通过响应缓存、智能路由、限流熔断等手段，让 AI 应用真正具备生产级可靠性。

## 核心特性

### 🌐 统一 API 层

- 兼容 OpenAI API 格式，现有代码几乎零改动即可接入；Anthropic SDK 走原生 `/v1/messages` 直通端点（thinking block 逐字节保真）
- 开箱即用支持 OpenAI、Anthropic、DeepSeek、通义千问、豆包等主流提供商
- 统一处理流式响应（SSE），屏蔽各家协议差异

### 🚀 高性能

- 单机 **5000+ QPS**，P99 延迟 **<20ms**（不含模型响应时间，[可复现脚本与方法](docs/benchmarks.md)）
- 基于 Go 的高并发实现，连接池 + 流式转发，内存占用 <500MB
- 异步计费与日志写入，不阻塞请求主路径

### 💰 成本控制

- **精确缓存**：相同请求直接命中 Redis 缓存，零上游成本
- **精确 token 计数**：内置 tokenizer，提供准确的成本统计（按 key、按用户、按模型）
- **智能路由**：规则引擎按任务复杂度选模型，支持按模型 glob + 权重百分比做金丝雀灰度（新模型 10% 流量试跑）

### 🛡️ 生产级可靠性

- **多维度限流**：令牌桶 + 滑动窗口，按请求数或 **token 数（TPM）** 计，key 可组合 api_key × model
- **自动重试 & 降级**：区分可重试错误，供应商故障时自动切换备用
- **Provider 热重载**：改 providers.yaml 后一个 API 调用整表原子生效，不重启、不断流
- **熔断保护**：避免单点故障级联放大

### 📊 完整可观测性

- Prometheus 指标：QPS、延迟、token 用量、缓存命中率、成本
- OpenTelemetry 分布式追踪：完整还原单次请求链路
- 结构化日志（JSON）：易于接入 ELK、Loki
- 预置 Grafana Dashboard，开箱即用

## 快速开始

### 零依赖启动（推荐）

网关默认是**无状态代理**，不需要 Postgres / Redis / Docker，一个二进制就能跑：

```bash
git clone https://github.com/An-idd/x-beacon.git
cd x-beacon
cp configs/providers.example.yaml configs/providers.yaml
# 编辑 providers.yaml，填入你的 OpenAI / Anthropic API key
make dev   # 首次运行会自动生成 configs/config.yaml（含静态 API key）
```

### 可选：启用 Postgres / Redis

需要 DB 管理 API key、请求日志与成本归因、响应缓存、跨实例限流时再加：

```bash
docker-compose up -d          # 启动 postgres + redis
xbctl migrate up              # 建表
# 然后在 config.yaml 里取消 database.dsn / redis.addr 的注释
```

服务启动后访问：

- 网关 API：`http://localhost:8080`
- 管理面板：见 [X-Beacon-Web](https://github.com/An-idd/X-Beacon-Web)（独立仓库，Vue 3 + Arco）
- Prometheus 指标：`http://localhost:8080/metrics`

### 发送第一个请求

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-local-dev-change-me" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "Hello, X-BEACON!"}]
  }'
```

切换模型只需改 `model` 字段，完全兼容 OpenAI SDK：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="sk-local-dev-change-me"
)

# 使用 Claude
resp = client.chat.completions.create(
    model="claude-3-5-sonnet",
    messages=[{"role": "user", "content": "Hello"}]
)

# 使用 DeepSeek，代码完全不变
resp = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "Hello"}]
)
```

### 本地源码编译

```bash
# 需要 Go 1.22+
make build                                 # 同时编 gateway 和 xbctl
./bin/x-beacon --config configs/config.yaml
```

### 启动期运维（Week 4 加入）

完整启动流程依赖 Postgres + Redis，由 `xbctl` 完成 schema 与首个 API key 的初始化：

```bash
# 1. 启动依赖
make docker-up

# 2. 应用 schema 迁移（嵌入二进制，无需仓库）
./bin/xbctl migrate up

# 3. 生成 API key（secret 仅打印一次，立即抓取）
./bin/xbctl keygen -name "local dev"
# 输出示例：
#   secret: sk-aBcD…46chars

# 4. 启动 gateway
./bin/x-beacon --config configs/config.yaml

# 5. 验证就绪
curl -s localhost:8080/readyz | jq .
# {"ready":true,"checks":{"postgres":{"ok":true},"redis":{"ok":true}}}
```

`xbctl` 子命令速查：


| 子命令                          | 用途                                    |
| ------------------------------- | --------------------------------------- |
| `xbctl migrate up|down|version` | schema 管理                             |
| `xbctl keygen -name <label>`    | 生成新 key（secret 仅打印一次）         |
| `xbctl keylist [-all] [-json]`  | 查看 key 列表                           |
| `xbctl keyrevoke -id <id>`      | 撤销 key（cache 最多 60s 内仍可能放行） |

### WebUI 本地联调（Week 13 加入）

`scripts/devup-webui.sh` 一键拉起后端 + mock upstream + 种子流量，专为
[X-Beacon-Web](https://github.com/An-idd/X-Beacon-Web) 前端联调准备：

```bash
scripts/devup-webui.sh           # 启动 docker、迁移、网关、mock、签发 admin key、灌入种子流量
scripts/devup-webui.sh stop      # 停掉 gateway + mockupstream（docker 保留）
```

脚本会按顺序完成：

1. `make docker-up` 启 Postgres + Redis-Stack，等端口就绪
2. `make build` 编译 `bin/x-beacon` + `bin/xbctl`
3. `xbctl migrate up` 应用 schema
4. 缺失时从 `configs/config.example.yaml` bootstrap `configs/config.yaml`，并写一份
   指向本地 mock upstream 的 `configs/providers.yaml`
5. 起 `scripts/mockupstream`（默认 `127.0.0.1:9091`）与 gateway（默认 `127.0.0.1:8080`）
6. `xbctl keygen` 签发一把带 `admin:webui` + `admin:pricing` scope 的 admin key
7. 灌 50 个成功 + 5 个 401 请求，让 Dashboard / Logs 页面有数据

运行结束后终端会打印 admin key 与快速校验命令，然后切到前端仓库 `npm run dev` 即可：

```bash
curl http://127.0.0.1:8080/healthz
curl -H "Authorization: Bearer <admin key>" http://127.0.0.1:8080/admin/stats/summary
```

可调环境变量：`DSN`、`GATEWAY_ADDR`、`MOCK_ADDR`、`TRAFFIC_OK`、`TRAFFIC_ERR`。
日志落在 `/tmp/xbeacon-devup/`。脚本**幂等**：重复执行会重启 gateway 以清掉熔断器
latched 状态，并以 unix 时间戳后缀生成新 key（旧 key 不自动清，按需
`xbctl keyrevoke`）。

详细部署说明见 [部署文档](docs/deployment.md)。

## 架构概览

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

完整的架构设计、关键决策和 trade-off 分析见 [architecture.md](docs/architecture.md)。

## 性能基准

在 AWS c6i.xlarge（4 vCPU, 8GB RAM）上的压测结果：


| 场景                 | QPS    | P50 延迟 | P99 延迟 | 说明              |
| -------------------- | ------ | -------- | -------- | ----------------- |
| 空请求（仅网关转发） | 8,200  | 1.2ms    | 4.8ms    | 不含模型响应      |
| 精确缓存命中         | 12,500 | 0.8ms    | 3.2ms    | Redis 缓存        |
| 限流检查             | 7,500  | 1.5ms    | 6ms      | 分布式限流        |

与同类项目对比：


| 项目         | 语言   | 空请求 P99 | 内存占用  |
| ------------ | ------ | ---------- | --------- |
| **X-BEACON** | **Go** | **4.8ms**  | **380MB** |
| LiteLLM      | Python | ~80ms      | ~1.2GB    |
| OneAPI       | Go     | ~15ms      | ~500MB    |

完整基准测试方法和数据见 [benchmarks.md](docs/benchmarks.md)。

## 兼容性矩阵

X-BEACON 把 OpenAI Chat Completions API 的 wire 格式做到字节级兼容,**只需把 `base_url` 指向网关即可**,客户端代码无需改动。字段级保真承诺(哪些字段保证透传、哪些字段网关会改写)见 [compatibility.md](docs/compatibility.md)。下面是每次 PR 都跑的回归覆盖范围(见 `.github/workflows/compat.yml`):

### L1 · Wire-level(纯 HTTP / cURL 视角)

| 场景                                | 锁定项                                              |
| ----------------------------------- | --------------------------------------------------- |
| 非流式 `/v1/chat/completions`       | JSON envelope 必备键 + 类型 + `Content-Type`        |
| 流式 SSE                            | `data: {...}\n\n` 帧分隔 + `data: [DONE]` 终止 + `Cache-Control: no-cache` + golden 字节锁定 |
| 错误响应 envelope                   | OpenAI shape `{"error":{"type","code","message"}}` + 401 / 400 两条路径 |
| `/v1/models` envelope               | `{"object":"list","data":[...]}` + 每条必备 `id` / `object` / `owned_by` |
| tool_call 响应                      | `arguments` 必须保持 JSON 字符串(不被二次编码)|
| usage token details                 | `prompt_tokens_details.cached_tokens` / `completion_tokens_details` 非流式 + 流式均原样透传 |

### L2 · OpenAI Python SDK(`openai>=1.55.0,<1.60.0`)

| SDK 方法 / 特性                                     | 覆盖                                            |
| --------------------------------------------------- | ----------------------------------------------- |
| `client.chat.completions.create(...)`               | basic / streaming                               |
| `client.chat.completions.create(tools=...)`         | tools + `tool_choice="required"`                |
| `client.chat.completions.create(response_format=)`  | `{"type":"json_object"}`                        |
| `client.chat.completions.create(logprobs=True)`     | logprobs 字段透传                               |
| `client.models.list()`                              | OpenAI 必备字段 + 对扩展字段(pricing / capabilities)的前向兼容 |
| Error class 映射                                    | `AuthenticationError` × 2 / `BadRequestError`   |

未覆盖矩阵(后续工作):TypeScript SDK(共用 OpenAPI codegen,边际价值低)/ LangChain `ChatOpenAI`(等真实用户反馈再补)/ Anthropic SDK(由 X-Beacon 的 OpenAI 兼容层覆盖,无独立 SDK 测试)。

### 本地复现

```sh
make compat-wire     # L1 Go 测试,无外部依赖
make compat-python   # L2 Python SDK,需要 uv (https://docs.astral.sh/uv/)
make compat          # 全跑
```

CI 上失败时 gateway / mockupstream 日志会被自动 dump。

## 使用案例

### 多供应商容灾

某客服系统接入 X-BEACON 的自动降级后，在 OpenAI 全球宕机的 2 小时内无缝切换到 Claude，业务零感知。

### 案例 3：精细化成本归因

某 SaaS 产品通过 X-BEACON 的 user 维度成本统计，识别出 0.3% 的用户消耗了 45% 的 token，随即调整定价策略。

## 路线图

### ✅ 已完成（v0.1 - MVP）

- [X]  统一 API 层（兼容 OpenAI 格式）
- [X]  支持 OpenAI、Anthropic、DeepSeek 三家 provider
- [X]  流式响应（SSE）
- [X]  API key 管理
- [X]  基础可观测性

### ✅ 已完成（v0.2 - 企业级特性）

- [X]  分布式限流（Redis 滑动窗口 + 内存令牌桶）
- [X]  自动重试与降级（full-jitter 指数退避 + 主备 chain）
- [X]  熔断器（per-provider gobreaker，4xx 不计 failure）
- [X]  Token 精确计数与成本统计（cl100k BPE + 异步 request_logs）
- [X]  Prometheus 指标完善（13 个核心 collector + Grafana dashboard）

### ✅ 已完成（v0.3 - 差异化亮点）

- [X]  精确缓存（Redis sha256 key + 4 条防污染门槛）
- [X]  智能路由（规则引擎：token 数 + 关键词，A/B opt-out via scope）
- [X]  Prompt 优化（system 永留 + 滑动窗口 + token 预算）

### 🚧 进行中（v0.4 - 管理面板）

- [X]  Admin API：CORS + `/admin/keys` + `/admin/logs` + `/admin/stats/{summary,timeseries}`
- [X]  只读端点：`/admin/routing/rules` / `/admin/providers` / `/admin/ratelimit/rules` / `/admin/cache/stats`
- [X]  审计日志（`admin_audit_logs`：key/pricing 变更，scope `admin:webui` 守门）
- [X]  Dashboard top-models 聚合
- [X]  Token 级限流（`unit: tokens`，TPM 按 prompt token 扣费）
- [X]  路由谓词扩展（model glob + weight 百分比，金丝雀灰度）
- [X]  Provider 配置热重载（`POST /admin/providers/reload`，整表原子替换）
- [X]  WebUI v0.2（[X-Beacon-Web](https://github.com/An-idd/X-Beacon-Web)：Vue 3 + Arco + TanStack Query，9 个页面）
- [ ]  WebUI 写功能完善（限流规则编辑、provider 健康操作）
- [ ]  多租户隔离

### 📋 计划中（v0.5 - 性能证明）

- [ ]  bench.sh 输出"网关净开销"单指标（对标 Bifrost 0.62ms / LiteLLM 5.83ms 的口径），`make bench` 一键复现
- [ ]  README 性能表全部数字换成该脚本的输出（附机器规格与 commit hash）
- [ ]  CI 可选 job：每次 release 跑一轮 bench 存档，防性能回归

### 💭 探索中

- [ ]  支持更多 provider（文心、Kimi、Gemini）
- [ ]  支持 function calling 标准化
- [ ]  支持多模态（图片、音频）
- [ ]  Python SDK

## 文档

- [架构设计](docs/architecture.md) - 系统架构、关键决策、权衡分析
- [性能基准](docs/benchmarks.md) - 压测方法与完整数据
- [兼容性承诺](docs/compatibility.md) - 字段级 wire 保真承诺,CI 回归锁定
- [运维手册](docs/runbook.md) - cache / 路由 / 压缩 / billing 常见运维动作
- [部署指南](docs/deployment.md) - 生产环境部署最佳实践
- [配置参考](docs/configuration.md) - 所有配置项说明

## 技术栈

- **语言**：Go 1.22+
- **路由**：chi
- **数据库**：PostgreSQL 16
- **缓存**：Redis 7
- **可观测性**：Prometheus + OpenTelemetry + Zap
- **部署**：Docker + Kubernetes

## 相关项目

- [LiteLLM](https://github.com/BerriAI/litellm) - Python 实现的类似项目，功能最全面
- [OneAPI](https://github.com/songquanpeng/one-api) - Go 实现的类似项目，国内使用较多
- [Portkey](https://github.com/Portkey-AI/gateway) - 商业化的 AI 网关

选择 X-BEACON 的理由：无状态轻量架构、更好的性能、更强的生产就绪度。详细对比见 [architecture.md](docs/architecture.md#与同类项目对比)。

## 协议

本项目基于 [Apache License 2.0](LICENSE) 开源。

## 致谢

- 感谢 [LiteLLM](https://github.com/BerriAI/litellm) 项目在 provider 抽象设计上的启发
- 感谢 [Envoy](https://github.com/envoyproxy/envoy) 的网关架构设计思想

---

<div align="center">

如果这个项目对你有帮助，请给它一个 ⭐️！

</div>
