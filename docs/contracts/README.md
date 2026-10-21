# 契约目录（W1 D1–D2 落库，API-first）

> 权威定义：根目录 [契约规范.md](../../契约规范.md) 与 [边界语义设计.md](../../边界语义设计.md)。
> 本目录是契约的**机器可读/样例形态**，与 `test/contract` 契约测试、`internal/core` Go 类型三者同源，
> 任何改动必须同步（CI 以契约测试守护）。当前开发代次允许破坏性 API/schema 调整，不提供旧版行为兼容层；
> **版本绑定在 run**；**幂等键贯穿全链路**；错误码稳定。

| 文件 | 内容 |
|---|---|
| [application-contract.v1.json](./application-contract.v1.json) | 应用契约 v1.0.0 机器可读定义：版本、错误信封、稳定 error code 全集 |
| [application-contract.md](./application-contract.md) | 应用契约兼容边界、Bots 最低版本、覆盖矩阵与测试入口 |
| [session-api.openapi.yaml](./session-api.openapi.yaml) | Session API（对外 REST + SSE + 动态 target）OpenAPI 3.0 定义 |
| [runs-protocol.md](./runs-protocol.md) | `/runs` 协议（worker ↔ harness 唯一协议）+ 请求/SSE 帧样例 |
| [executor-protocol.md](./executor-protocol.md) | Executor 协议（worker ↔ executor）+ 生命周期与幂等语义 |
| [events.schema.json](./events.schema.json) | 事件 schema（append-only，类型全集枚举） |

样例数据（Go 契约测试读取）：

- `../../test/contract/testdata/runs.request.json` — /runs 请求样例
- `../../test/contract/testdata/runs.frames.ndjson` — SSE 帧序列样例（全部工具、参数错误及 done）
- `../../test/contract/testdata/events.sample.json` — 事件行样例
- `../../test/contract/testdata/application.bridge-call.json` — 应用工具 bridge 信封
- `../../test/contract/testdata/application.timer-fire.json` — timer fire 信封

## 版本与 Changelog（M4，2026-10-09）

| 契约 | 版本 | 变更 |
|---|---|---|
| runs-protocol | v1.0 | 初始（protocol 1.0、三铁律、幂等 (run_id,step)） |
| executor-protocol | v1.0 | 初始（六端点 + capabilities Seam 声明） |
| events.schema | v1.0 | 初始（类型全集只增不改）+ M1 哈希链字段（prev_hash/event_hash/hold） |
| seam | v1.0 | 新增（可替换接缝的一等公民声明） |
| application-contract | v1.0.0 | 稳定错误信封/码表、`X-Chronotope-Application-Contract`、动态 bridge/timer target、timer `last_error_code` |

2026-10-19 应用 profile：创建幂等、run 202/GET、全部工具交棒与原 call ID、固定 bridge、通用 timer。
2026-10-21 P0：应用契约 v1.0.0；错误码/分类/retryable 稳定；`test/contract` + Bots-like
receiver 覆盖 agent/session/run、bridge、timer、tenant、幂等与错误；动态 target 支持
运行时注册与 digest fencing。历史场景不自动构成当前门禁证据。

OpenAPI `info.version` 是文档修订号；Bots 应读取 `info.x-application-contract-version`
和响应头 `X-Chronotope-Application-Contract`，不要用 `info.version` 判断应用契约兼容性。
