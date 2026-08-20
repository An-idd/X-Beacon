# 兼容性承诺（Wire Fidelity Contract）

"OpenAI 兼容"是一个光谱，不是复选框。业界大量网关会静默丢弃它们没有建模的字段——
`prompt_tokens_details.cached_tokens`（prompt 缓存上报）、`logprobs`、流式 tool_call
delta 都是重灾区。本页是 X-BEACON 对 `/v1/chat/completions` 的字段级保真承诺，
每一条都有 CI 回归测试锁定（`test/wire_compat_test.go` + `.github/workflows/compat.yml`）。

## 原则

网关只对**它语义上需要理解的字段**建模；其余一切字段通过 `Extra` 机制
（`map[string]json.RawMessage`）在解析时原样保留、序列化时原样回写。
**未知字段永远不会被丢弃**——包括 OpenAI 未来新增的字段。

## 保证原样透传（request → upstream）

| 字段 | 说明 |
|------|------|
| `tools` / `tool_choice` | 完整透传，不解析不改写 |
| `response_format` | 含 `json_object` / `json_schema` |
| `logprobs` / `top_logprobs` | |
| `logit_bias`、`seed`、`presence_penalty` 等一切采样参数 | |
| `stream_options` | 含 `include_usage` |
| `messages[].tool_calls`、`tool_call_id`、多模态 content | message 级 Extra |
| 任何未列出的未知字段 | Extra 机制兜底 |

## 保证原样透传（upstream → client）

| 字段 | 锁定测试 |
|------|----------|
| `usage.prompt_tokens_details`（含 `cached_tokens`） | `TestWireCompat_NonStreaming_UsageDetailsPassthrough` |
| `usage.completion_tokens_details`（含 `reasoning_tokens`） | 同上 |
| 流式最终 chunk 的 usage details | `TestWireCompat_Streaming_UsageDetailsPassthrough` |
| `choices[].message.tool_calls`（`arguments` 保持 JSON 字符串，不二次编码） | `TestWireCompat_NonStreaming_ToolCallArgumentsAreString` |
| `choices[].logprobs` / `refusal` / `audio` | choice / message 级 Extra |
| 流式 delta 中的 `tool_calls` / `refusal` 增量 | delta 级 Extra |
| SSE 帧结构：`data: {...}\n\n` 分隔 + `data: [DONE]` 终止 | `TestWireCompat_Streaming_SSELayout`（golden 字节锁定） |
| 错误 envelope：`{"error":{"type","code","message"}}` | `TestWireCompat_ErrorEnvelope_*` |

## 网关会改写的字段（明确列出，别处一律不动）

| 字段 | 何时改写 | 如何关闭 |
|------|----------|----------|
| `model` | 智能路由规则命中时（响应 `model` 如实反映实际服务的模型） | `routing.enabled: false`，或 per-key scope `smart_route:disable` |
| `messages`（截断） | prompt 压缩触发时（超过 context window 的 `trigger_ratio`） | `prompt.compression.enabled: false` |

改写发生时通过响应头显式告知，客户端可以审计：

| 响应头 | 含义 |
|--------|------|
| `X-X-Beacon-Route-Rule` / `-Route-From` / `-Route-To` | 哪条路由规则、从什么模型改到什么模型 |
| `X-X-Beacon-Prompt-Compressed: 1` | 本次请求的历史消息被截断过 |
| `X-X-Beacon-Cache: hit\|miss` | 响应是否来自精确缓存 |

## Anthropic 原生端点 `/v1/messages`

Anthropic 协议的保真策略更彻底：**逐字节直通**。网关只窥探 `model`
（路由）和 `stream`（响应处理）两个字段，请求体原样转发、响应体
（JSON 或 SSE）原样回传——thinking block、`signature_delta`、prompt
caching 字段、未来新增的任何协议特性都不会损坏，因为网关根本不解析。

- 鉴权：`x-api-key`（Anthropic SDK 默认）或 `Authorization: Bearer`，
  同一套网关 key
- `anthropic-beta` 请求头原样透传
- 上游错误 envelope + `Retry-After` 原样透传
- usage（`input_tokens`/`output_tokens`）旁路解析用于计费，不改动响应
- 锁定测试：`internal/server/messages_test.go`（含 thinking 流式逐字节断言）

限制：仅路由到 anthropic 类型 provider（跨协议转换请走
`/v1/chat/completions`）；此路径不经过缓存/智能路由/prompt 压缩/重试
熔断——直通与改写不可兼得，保真优先。

## 已知边界

- OpenAI 兼容路径上的 Anthropic 上游走协议转换，Anthropic 专有 usage
  字段（如 `cache_creation_input_tokens`）在该路径暂不透传——需要
  完整保真请改用原生 `/v1/messages`
- 请求头除 `Authorization` / `x-api-key`（替换为 provider key）和
  `anthropic-beta` 外不透传自定义头
