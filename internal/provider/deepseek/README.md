# internal/provider/deepseek

DeepSeek 适配器。DeepSeek 实现 OpenAI 兼容的 chat API，所以这里是一个对 [`openai.Provider`](../openai/) 的薄包装。

## 对外 API

```go
p, err := deepseek.New(deepseek.Config{
    Name:     "deepseek",
    Endpoint: "https://api.deepseek.com",  // 可选，此为默认
    APIKey:   os.Getenv("DEEPSEEK_API_KEY"),
    Timeout:  60 * time.Second,
    Models: openai.Models{
        Exact: []string{"deepseek-chat", "deepseek-reasoner"},
    },
})
```

返回的是 `*openai.Provider`，因为底层协议就是 OpenAI 格式。

## 为什么是独立包而不是 `type: openai` + endpoint 覆盖

单独起一个 `type: deepseek` 让：

- **指标/日志归属清晰** ——`provider` label 区分直接调用 OpenAI 还是 DeepSeek
- **未来分叉的落地点** ——`deepseek-reasoner` 的 `reasoning_content` 字段、潜在错误码差异等未来可在本包加逻辑，而不是污染通用 openai 适配器

代码成本只有 ~30 行（主要是默认 endpoint 和字段转发）。

## Week 1 假设验证

`stream.go` 认为"SSE 流未收到 `[DONE]` 即为截断错误"。DeepSeek 遵循 OpenAI 格式，**应该**发送 `[DONE]`。本包测试用两个用例覆盖：

- `TestStreaming_WithDoneMarker` —— DeepSeek 发 [DONE] 的正常路径
- `TestStreaming_WithoutDoneMarker_EmitsError` —— 不发 [DONE] 时当前行为（emit `ErrUpstream`）

真实 DeepSeek API 验证需要 `DEEPSEEK_API_KEY`；若实际行为违反假设，在 [stream.go](../openai/stream.go) 加 per-provider 的 `require_done_marker` 开关。

## 不做的事

- **不重复实现 ChatCompletion / ChatCompletionStream** ——全部交给 `openai` adapter
- **不处理 `reasoning_content` 字段** ——`deepseek-reasoner` 的扩展字段，未来需要时本包加独立 wire 类型
- **不独立设置 User-Agent** ——沿用 openai 的 `"x-beacon"`
