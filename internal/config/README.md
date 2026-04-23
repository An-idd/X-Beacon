# internal/config

加载和校验网关配置。

## 对外 API

```go
cfg, err := config.Load("configs/config.yaml")
```

- 空 path 只读默认值 + 环境变量
- 环境变量前缀 `XBEACON_`，`.` → `_`（如 `XBEACON_SERVER_ADDR` 覆盖 `server.addr`）
- 返回前会调用 `Validate()`，不通过的配置不会返回 `*Config`

## 新增配置项的步骤

1. 在 `Config` 或子结构里加字段，带 `mapstructure` tag
2. 在 `setDefaults()` 里给默认值
3. 在 `Validate()` 里加约束（若有）
4. 在 `configs/config.example.yaml` 里补示例
5. 如需 env 覆盖，**不需要**手动 `BindEnv`——`AutomaticEnv` + `SetDefault` 组合自动生效

## 约束

- 所有时间字段用 `time.Duration`，Viper 自动解析 `"10s"` / `"30m"`
- 敏感字段（API key、密码）不要有默认值，必须强制显式配置
- `RateLimitRule` 结构 Week 5 细化，目前是占位
