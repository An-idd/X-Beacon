# internal/provider/registry

从 `configs/providers.yaml` 加载、校验并构造 provider 适配器；对外提供按名 / 按模型的解析能力。

## 对外 API

```go
reg, err := registry.Load("configs/providers.yaml")
if err != nil { /* 启动失败；main 决定退出 */ }

p, err := reg.GetByName("openai-primary")
p, err := reg.ResolveModel("gpt-4o-mini")
names := reg.Names()
models := reg.AllModels()  // 用于 /v1/models
```

## 独立子包的理由

为避免 `internal/provider` → `internal/provider/openai` → `internal/provider`（接口引用）的导入循环，registry 单独起一个子包，让它自由 import 各 adapter。

## 解析优先级

```
exact match > first glob match (declaration order) > default_provider
```

任一层命中即返回；全部落空 → `ErrNoProviderForModel`。详见 [TODO.md / phase.md Step 1.3 决策](../../../phase.md)。

## 启动校验

Load 期间会聚合所有错误（`errors.Join`），一次性暴露多个问题：

- `name` / `type` 必填
- 至少一个 model（exact 或 glob）
- provider name 不能重复
- exact model 不能跨 provider 冲突（显式报告 owner）
- glob 模式必须是合法的 `path.Match` 模式
- `default_provider` 指定的 name 必须已注册

校验失败 → `Load` 返回错误；**不 panic**。main 决定退出策略。

## 环境变量展开

YAML 加载前对整个文件内容做文本替换。支持：

| 语法 | 行为 |
|---|---|
| `${VAR}` | `os.Getenv("VAR")`；未设置 / 空时 → 空字符串 |
| `${VAR:-default}` | 未设置 / 空时 → `default` |
| `$VAR`（无大括号） | **不展开**，按字面字符处理（避免与成本 pattern 占位符歧义） |

空白或特殊字符在 default 中均可（如 `${X:-http://host:8080}`）。

## 添加新 provider 类型的步骤

1. 在 `internal/provider/<vendor>/` 写 adapter（实现 `provider.Provider`）
2. [loader.go](loader.go) 的 `constructProvider` switch 加一条 case
3. [providers.example.yaml](../../../configs/providers.example.yaml) 加示例节（注释掉，由用户按需启用）

## 不做的事

- **不做热重载**——Registry 是启动后不可变数据；修改 providers.yaml 需要重启进程
- **不做 provider 级重试/熔断**——Week 6 router 层统一做
- **不做 `priority` 使用**——YAML schema 里的 `priority` 只被读取保存；Week 6 降级逻辑才会消费它
