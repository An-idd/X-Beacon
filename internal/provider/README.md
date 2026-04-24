# internal/provider

LLM 提供商的统一抽象层。每个上游（OpenAI、Anthropic、DeepSeek 等）实现 `Provider` 接口；请求/响应结构使用 OpenAI 兼容字段名作为"通用格式"，由各 provider 内部做双向转换。

## 对外 API

### 接口

```go
type Provider interface {
    Name() string
    ChatCompletion(ctx, req) (*ChatResponse, error)
    ChatCompletionStream(ctx, req) (<-chan StreamEvent, error)
    SupportedModels() []ModelInfo
}
```

### 数据结构

- `ChatRequest` — 最小可用 9 字段（`model / messages / max_tokens / temperature / top_p / n / stream / stop / user`）+ `Extra map[string]json.RawMessage` 透传客户端提供的未知字段
- `ChatResponse` / `Choice` / `Usage` — 非流式响应
- `ChatStreamChunk` / `StreamChoice` / `MessageDelta` — 流式 chunk
- `StreamEvent` — 流式 channel 的元素；`{Chunk, Err}` 互斥，第一个非 nil `Err` 之后 channel 关闭
- `ModelInfo` — `/v1/models` 端点的元数据

### 错误

Sentinel：`ErrAuth` / `ErrRateLimited` / `ErrContextLength` / `ErrInvalidRequest` / `ErrUpstream` / `ErrUnavailable` / `ErrTimeout`

Provider 实现者必须把上游的原生错误归一化为其中之一，通过 `NewUpstreamError` 包装后返回：

```go
return nil, provider.NewUpstreamError("openai-primary", provider.ErrRateLimited, 429, "slow down")
```

调用方两种用法：
```go
if errors.Is(err, provider.ErrRateLimited) { /* 分类判断 */ }

var ue *provider.UpstreamError
if errors.As(err, &ue) { /* 需要 StatusCode / RetryAfter 时 */ }
```

`IsRetryable(err)` 一行判断是否应该重试（`RateLimited / Upstream / Unavailable / Timeout` 返回 true；`Auth / ContextLength / InvalidRequest` 返回 false）。

## 设计约束

- **接口小而稳定**：Week 1 只含 Chat；Embeddings 和 HealthCheck 按实际需要（Week 6 降级需要 HealthCheck）再扩展——避免没实现就先声明
- **流式用 channel**：producer 在终止 event 之后关闭 channel；consumer `for ev := range ch` 即可
- **Extra 单向优先**：`UnmarshalJSON` 时已从 Extra 中剔除所有已知 key；`MarshalJSON` 时即使 Extra 里有同名 key 也会被结构体字段覆盖
- **Temperature / TopP 用 `*float64`**：区分"未设置"和"值为 0"（后者是合法且语义重要的值）
- **String() 不泄敏**：`ChatRequest.String()` 只输出 model + message 数量 + stream 标志，不打印 prompt 内容（遵守 CLAUDE.md "日志不打完整 prompt"）

## 不做的事

- 不提供 embedding 类型（未到用时不设计）
- 不支持多模态 content（`Message.Content` 只是 string，数组形式留给 Phase 3）
- 不提供默认重试/超时实现（每个 provider 有自己的策略，靠 `IsRetryable` 配合 Week 6 的 retry middleware）
