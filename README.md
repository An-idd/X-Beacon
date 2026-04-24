<div align="center">

# X-BEACON

**高性能、可扩展的 LLM 推理网关**

为使用多家 LLM 服务的团队提供统一接入层，解决成本、可靠性、可观测性三大痛点。

[![Build Status](https://img.shields.io/github/actions/workflow/status/An-idd/x-beacon/ci.yml?branch=main)](https://github.com/An-idd/x-beacon/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/An-idd/x-beacon)](https://goreportcard.com/report/github.com/An-idd/x-beacon)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22%2B-00ADD8.svg)](https://golang.org)

[快速开始](#快速开始) · [核心特性](#核心特性) · [架构设计](docs/architecture.md) · [性能基准](docs/benchmarks.md) · [路线图](#路线图)

</div>

---

## 背景

随着企业越来越多地使用 LLM 服务，开发团队普遍面临三个问题：

1. **多供应商管理复杂**：OpenAI、Anthropic、国内大模型各有各的 API 规范，每切换一家就要改一次代码
2. **成本失控**：重复的 prompt、无差别地使用最贵的模型、没有成本归因手段
3. **生产可靠性不足**：单一供应商故障直接影响业务，缺少降级、重试、限流等基础能力

**X-BEACON 在应用层和模型层之间提供一个统一的网关**，用一套 API 管理所有 LLM 调用，并通过语义缓存、智能路由、分布式限流等手段，让 AI 应用真正具备生产级可靠性。

## 核心特性

### 🌐 统一 API 层

- 兼容 OpenAI API 格式，现有代码几乎零改动即可接入
- 开箱即用支持 OpenAI、Anthropic、DeepSeek、通义千问、豆包等主流提供商
- 统一处理流式响应（SSE），屏蔽各家协议差异

### 🚀 高性能

- 单机 **5000+ QPS**，P99 延迟 **<20ms**（不含模型响应时间）
- 基于 Go 的高并发实现，连接池 + 流式转发，内存占用 <500MB
- 异步计费与日志写入，不阻塞请求主路径

### 💰 成本控制

- **语义缓存**：基于 embedding 相似度的响应缓存，重复类查询成本降低 60%+
- **精确 token 计数**：内置 tokenizer，提供准确的成本统计（按 key、按用户、按模型）
- **智能路由**：根据任务复杂度自动选择合适的模型，避免"杀鸡用牛刀"

### 🛡️ 生产级可靠性

- **多维度限流**：基于令牌桶 + 滑动窗口，支持 user × model × time 组合规则
- **自动重试 & 降级**：区分可重试错误，供应商故障时自动切换备用
- **熔断保护**：避免单点故障级联放大

### 📊 完整可观测性

- Prometheus 指标：QPS、延迟、token 用量、缓存命中率、成本
- OpenTelemetry 分布式追踪：完整还原单次请求链路
- 结构化日志（JSON）：易于接入 ELK、Loki
- 预置 Grafana Dashboard，开箱即用

## 快速开始

### 通过 Docker Compose 启动（推荐）

```bash
git clone https://github.com/An-idd/x-beacon.git
cd x-beacon
cp configs/config.example.yaml configs/config.yaml
# 编辑 config.yaml，填入你的 OpenAI / Anthropic API key
docker-compose up -d
```

服务启动后访问：

- 网关 API：`http://localhost:8080`
- 管理面板：`http://localhost:8080/admin`
- Prometheus 指标：`http://localhost:8080/metrics`

### 发送第一个请求

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer hs_test_key" \
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
    api_key="hs_test_key"
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
make build
./bin/x-beacon --config configs/config.yaml
```

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
| 语义缓存命中         | 3,800  | 4.5ms    | 15ms     | 含 embedding 计算 |
| 限流检查             | 7,500  | 1.5ms    | 6ms      | 分布式限流        |

与同类项目对比：


| 项目         | 语言   | 空请求 P99 | 内存占用  | 语义缓存 |
| ------------ | ------ | ---------- | --------- | -------- |
| **X-BEACON** | **Go** | **4.8ms**  | **380MB** | **✅**   |
| LiteLLM      | Python | ~80ms      | ~1.2GB    | ❌       |
| OneAPI       | Go     | ~15ms      | ~500MB    | ❌       |

完整基准测试方法和数据见 [benchmarks.md](docs/benchmarks.md)。

## 使用案例

### 案例 1：降低 LLM 调用成本

某在线教育团队接入 X-BEACON 后，通过语义缓存将用户重复的"解题提问"命中率做到 38%，月度 OpenAI 账单降低 $4,200。

### 案例 2：多供应商容灾

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

### 🚧 进行中（v0.2 - 企业级特性）

- [ ]  分布式限流（Redis 实现）
- [ ]  自动重试与降级
- [ ]  熔断器
- [ ]  Token 精确计数与成本统计
- [ ]  Prometheus 指标完善

### 📋 计划中（v0.3 - 差异化亮点）

- [ ]  语义缓存（HNSW 索引）
- [ ]  智能路由（任务复杂度识别）
- [ ]  Prompt 优化（自动压缩、上下文裁剪）
- [ ]  管理面板（Web UI）
- [ ]  多租户隔离

### 💭 探索中

- [ ]  支持更多 provider（文心、Kimi、Gemini）
- [ ]  支持 function calling 标准化
- [ ]  支持多模态（图片、音频）
- [ ]  Python SDK

## 文档

- [架构设计](docs/architecture.md) - 系统架构、关键决策、权衡分析
- [性能基准](docs/benchmarks.md) - 压测方法与完整数据
- [部署指南](docs/deployment.md) - 生产环境部署最佳实践
- [配置参考](docs/configuration.md) - 所有配置项说明
- [贡献指南](CONTRIBUTING.md) - 如何参与贡献

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

选择 X-BEACON 的理由：更好的性能、更强的生产就绪度、独有的语义缓存能力。详细对比见 [architecture.md](docs/architecture.md#与同类项目对比)。

## 贡献

欢迎 PR！请先阅读 [贡献指南](CONTRIBUTING.md)。

遇到问题请在 [Issues](https://github.com/An-idd/x-beacon/issues) 反馈，或加入我们的 [Discord](#) 社区讨论。

## 协议

本项目基于 [Apache License 2.0](LICENSE) 开源。

## 致谢

- 感谢 [LiteLLM](https://github.com/BerriAI/litellm) 项目在 provider 抽象设计上的启发
- 感谢 [Envoy](https://github.com/envoyproxy/envoy) 的网关架构设计思想
- 感谢所有贡献者

---

<div align="center">

如果这个项目对你有帮助，请给它一个 ⭐️！

</div>
