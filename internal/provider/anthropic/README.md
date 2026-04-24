# internal/provider/anthropic

Anthropic Messages API 适配器。与 DeepSeek 不同，Anthropic 的 wire 协议**不是** OpenAI 兼容，所以本包维护独立的类型 + 双向转换层。

Step 2.2 实现非流式；Step 2.3 完成流式（见下方"流式"章节）。

## 对外 API

```go
p, err := anthropic.New(anthropic.Config{
    Name:             "anthropic-primary",
    Endpoint:         "https://api.anthropic.com",  // 默认即此
    APIKey:           os.Getenv("ANTHROPIC_API_KEY"),
    APIVersion:       "",                            // 默认 "2023-06-01"
    Timeout:          60 * time.Second,
    DefaultMaxTokens: 4096,                          // 调用方未传 max_tokens 时使用
    Models: anthropic.Models{
        Exact: []string{"claude-3-5-sonnet-20241022", "claude-3-haiku-20240307"},
        Glob:  []string{"claude-*"},
    },
})

resp, err := p.ChatCompletion(ctx, req)        // 非流式
ch, err := p.ChatCompletionStream(ctx, req)    // 流式，for ev := range ch
```

`*Provider` 编译期满足 `provider.Provider`（`var _ provider.Provider = (*Provider)(nil)` 在 [anthropic_test.go](anthropic_test.go) 断言）。

## 与 OpenAI 格式的差异 + 本包如何处理

### 请求

| 字段 | OpenAI 格式 | Anthropic 格式 | 本包转换 |
|---|---|---|---|
| system 提示 | `{"role":"system",...}` 在 messages | 顶层 `"system": "..."` | 扫描 messages 中所有 `role=system`，`\n\n` 拼接到顶层 |
| max_tokens | 可选（默认模型上限） | **必填** | 缺省时填 `DefaultMaxTokens`（默认 4096） |
| stop | string 或 []string | `stop_sequences: []string` | 直接映射（`provider.ChatRequest.Stop` 已是 `[]string`） |
| n | 支持 | 不支持 | 静默忽略（Anthropic 只返回 1 个） |

### 响应

| 字段 | OpenAI 格式 | Anthropic 格式 | 本包转换 |
|---|---|---|---|
| content | `choices[].message.content: string` | `content: [{"type":"text","text":"..."}]` 数组 | 拼接所有 `type=text` 块；非 text 块静默跳过（forward-compat） |
| finish_reason | "stop" / "length" / "tool_calls" | `stop_reason`: "end_turn" / "max_tokens" / "stop_sequence" / "tool_use" | 映射表见 [convert.go](convert.go) `mapStopReason` |
| usage 字段名 | `prompt_tokens / completion_tokens / total_tokens` | `input_tokens / output_tokens` | 转换 + 自己计算 total |
| created | 有 | **无** | 合成 `time.Now().Unix()`，让 OpenAI 客户端能读 |

### 鉴权

- **头**：`x-api-key: <key>`（**不是** `Authorization: Bearer`）
- **强制头**：`anthropic-version: 2023-06-01`（可通过 `Config.APIVersion` 覆盖）

### 错误分类

Anthropic 的 `error.type` 比 HTTP status 更权威，因此 `mapToSentinel` 先看 type 再看 status。特别注意：

- **`overloaded_error`** → `ErrUnavailable`（retryable）；Anthropic 有时以 HTTP 529 返回
- **`request_too_large`** → `ErrContextLength`
- **429 + overloaded_error** 都会解析 `Retry-After` 头

## `stop_reason` 映射表

| Anthropic | 映射到 |
|---|---|
| `end_turn` | `stop` |
| `max_tokens` | `length` |
| `stop_sequence` | `stop` |
| `tool_use` | `tool_calls`（Week 2 还不支持 tools） |
| 其他 | 原样透传 |

## 流式（Step 2.3）

Anthropic 的 SSE 与 OpenAI 截然不同：多种 `event:` 类型，元数据只在首帧 `message_start`。本适配器把多 event 流重组成 OpenAI 风格的 `ChatStreamChunk` 序列。

### 发出的 chunk 序列

```
message_start        →  chunk: {delta: {role: "assistant"}}
content_block_delta  →  chunk: {delta: {content: "..."}}  ← 每个 text_delta 一帧
content_block_delta  →  chunk: {delta: {content: "..."}}
...
message_delta        →  chunk: {delta: {}, finish_reason: "stop", usage: {...}}
message_stop         →  close(ch)
```

首帧后的每个 chunk 都复用 `message_start` 的 `id / model`（经由 `anthStreamState` 缓存）。

### Event 分派表

| Anthropic event | 行为 |
|---|---|
| `message_start` | 缓存 id/model/input_tokens；发 role chunk |
| `content_block_start` | no-op |
| `content_block_delta` (text_delta) | 发 content chunk |
| `content_block_delta` (input_json_delta) | **跳过**（MVP 不支持 tool_use） |
| `content_block_stop` | no-op |
| `message_delta` | 发 finish chunk（含 finish_reason + 完整 usage） |
| `message_stop` | close(ch)，相当于 `[DONE]` |
| `ping` | no-op（keepalive） |
| `error` | emit `StreamEvent{Err}` + close(ch) |
| 未知 type | **跳过**（forward-compat） |

### SSE 解析

忽略 `event:` 行，只解析 `data:`。JSON payload 里的 `type` 字段是分派唯一依据（单一真相源）。

### 5 条退出路径（对齐 openai/stream.go）

| 路径 | 结果 |
|---|---|
| `message_stop` | close，无事件 |
| ctx 取消 | close，无事件 |
| `error` event | `StreamEvent{Err}` + close |
| JSON 解析失败 | `StreamEvent{Err: ErrUpstream}` + close |
| EOF 但无 `message_stop` | `StreamEvent{Err: ErrUpstream}` + close |

`defer close(ch); defer body.Close()` 兜底所有分支。

### 语义映射到 OpenAI 的取舍

- **`choices[0].index` 固定为 0**，不沿用 Anthropic content_block 的 index——后者是块内结构位置，OpenAI 的 index 保留给 `n>1` 的并行补全
- **Usage 放终止 chunk 内**（与 finish_reason 同帧）——Anthropic 只在 message_delta 给最终 `output_tokens`；合并一次给全

## 目前不做的事

- **不支持 multimodal 内容** —— `Message.Content` 仅 string，image/document 块延后
- **不支持 tool_use 流程** —— `tool_calls` 在 finish_reason 里能识别，但不解析/透传 tool_use content blocks
- **不支持 `top_k`** —— Anthropic 独有，`provider.ChatRequest` 没对应字段；未来可通过 `Extra` 透传
- **不做重试** —— Week 6 router 层统一

## 测试

- `httptest.NewServer` 伪造 Anthropic；**所有错误路径** 验证 `errors.Is(sentinel)` + `provider.IsRetryable` 分类
- [convert_test.go](convert_test.go) 独立测试转换函数（与 HTTP 无关），确保 system 提取 / max_tokens 默认 / content 拼接等逻辑不被 chat.go 的测试稀释
- `TestChatCompletion_ContextCanceled` 用 pre-cancelled ctx 避免 httptest 死锁（Week 1 Step 1.2a 踩过的坑）
