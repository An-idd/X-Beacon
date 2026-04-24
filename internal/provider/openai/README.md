# internal/provider/openai

OpenAI（以及 OpenAI 兼容端点，如 Azure OpenAI、DeepSeek）的适配器实现。

## 对外 API

```go
p, err := openai.New(openai.Config{
    Name:         "openai-primary",
    Endpoint:     "https://api.openai.com",  // 可选，默认即此
    APIKey:       os.Getenv("OPENAI_API_KEY"),
    Organization: "",                          // 可选
    Timeout:      60 * time.Second,           // 非流式超时；默认 60s
    Models: openai.Models{
        Exact: []string{"gpt-4o", "gpt-4o-mini"},
        Glob:  []string{"gpt-4-*", "gpt-3.5-*"},
    },
})

resp, err := p.ChatCompletion(ctx, req)
```

`*Provider` 满足 [`provider.Provider`](../provider.go) 接口（编译期 `var _ provider.Provider = (*Provider)(nil)` 断言保证）。

## 设计要点

- **单个 `*http.Client`，`Timeout: 0`**——由 ctx 控制生命周期，使同一 client 可同时服务流式/非流式
- **`Timeout=0` 不等于无超时**——非流式在 `ChatCompletion` 内部用 `context.WithTimeout(ctx, cfg.Timeout)` 包裹
- **`Stream` 标志由方法名决定**，用户传入的 `req.Stream` 被覆盖；避免调用方传错参数报错
- **错误归一化按表映射**（见 `errors.go`）：先 HTTP status 粗分类，再用 OpenAI `error.code` 区分 context_length
- **`SupportedModels()` 只返回 `Models.Exact`**——glob 是动态路由规则，不适合列进 `/v1/models`
- **Retry-After 只在 429 时解析**——其他 status 不携带此语义

## 错误映射表

| HTTP status | OpenAI `error.code` | 映射到 sentinel |
|---|---|---|
| 401 / 403 | any | `ErrAuth` |
| 429 | any | `ErrRateLimited`（+ 解析 `Retry-After`） |
| 400 | `context_length_exceeded` | `ErrContextLength` |
| 400 / 404 / 422 | 其他 | `ErrInvalidRequest` |
| 408 | — | `ErrTimeout` |
| 503 | — | `ErrUnavailable` |
| 5xx / 未知 4xx | — | `ErrUpstream` |
| `context.Canceled` | — | 原样返回，不包 |
| `context.DeadlineExceeded` | — | `ErrTimeout`（retryable） |
| 其他 `net.Error` | — | `ErrUnavailable`（retryable） |

## 流式（ChatCompletionStream）

```go
ch, err := p.ChatCompletionStream(ctx, req)
if err != nil { /* HTTP 请求失败，在首帧前 */ }
for ev := range ch {
    if ev.Err != nil { /* 流式期间的终止错误 */ ; break }
    useChunk(ev.Chunk)
}
```

### SSE 解析约定

- `bufio.Scanner` 读行，缓冲上限 1 MiB
- 识别 4 种行：空行（事件边界）/ `:` 开头（注释/keepalive，忽略）/ `data: <json>` / `data: [DONE]`
- 兼容 `data:` 后无空格的变体（SSE 规范中空格可选）

### Goroutine 退出路径（5 条，全部 `defer close(ch); defer body.Close()` 兜底）

| 路径 | channel 结果 |
|---|---|
| 收到 `[DONE]` | close，无事件 |
| ctx 取消 | close，无事件（尊重调用方意图） |
| chunk JSON 解析失败 | `StreamEvent{Err: ErrUpstream}` + close |
| scanner I/O 错误 | `StreamEvent{Err: ErrUpstream}` + close |
| 未收到 `[DONE]` 就 EOF | `StreamEvent{Err: ErrUpstream}` + close（流被截断） |

### 流式与超时

- `cfg.Timeout` **不应用**到流——流体可长达分钟级
- 由调用方 ctx 决定流的最大生命周期
- 首帧前的 HTTP 请求（`Do()`）仍受 ctx 约束，4xx/5xx 同步返回错误

### Channel 规格

- Buffered=16——平衡 smoothing 与慢消费者暴露
- Producer 负责 close；consumer `for ev := range ch` 即可
- 任何 goroutine 泄漏测试（`TestStream_ClientCancels`）都以"channel 是否在 3s 内关闭"为判据

## 不做的事

- **不做重试**——由 Week 6 router 层用 `provider.IsRetryable()` 统一处理，避免双层重试
- **不在流中间重试**——即便首帧后遇到 `ErrUpstream`，也不重试（已部分消费响应不幂等）
- **不检查 `req.Model` 是否在 `Models.Exact/Glob` 内**——路由 + 校验是 registry 的职责（Step 1.3）

## 测试

- `httptest.NewServer` 伪造 OpenAI，不真调任何外部服务（CLAUDE.md "测试中不调真实 API"）
- 所有错误路径验证两点：`errors.Is(..., sentinel)` + `provider.IsRetryable` 分类正确
- `ContextCanceled` 测试显式等服务端观测到客户端断开，防止测试过早结束放过 goroutine 泄漏
