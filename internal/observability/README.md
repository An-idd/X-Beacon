# internal/observability

提供日志、指标、追踪三件套的构造函数。调用方负责持有返回对象并在进程退出前调用 shutdown。

## 对外 API

```go
logger, err := observability.NewLogger(observability.LogConfig{Level: "info", Format: "json"})
reg := observability.NewMetricsRegistry()
tp, shutdown, err := observability.NewTracerProvider(ctx, observability.TracingConfig{...})
defer shutdown(ctx)
```

## 设计要点

- **Logger**：Zap production 配置为基，ISO8601 时间戳、`ts` 字段名。支持 `json` 和 `console` 两种 encoding
- **Metrics**：预注册 Go runtime + process collectors；业务指标由各模块自行注册到返回的 `*prometheus.Registry`
- **Tracing**：
  - 未启用时仍返回非 nil `*TracerProvider`，以便上层代码无条件调用 OTel API
  - 启用时用 OTLP HTTP 导出器（默认 insecure，生产需换配置）
  - Resource 用 `resource.NewSchemaless` 避开不同 OTel 版本的 schema URL 冲突

## 不做的事

- 不在本包里提供全局 logger 单例——main 负责装配后向下依赖注入
- 不提供业务指标的定义——每个模块定义自己关心的指标
