# 契约目录（W1 D1–D2 落库，API-first）

> 权威定义：根目录 [契约规范.md](../../契约规范.md) 与 [边界语义设计.md](../../边界语义设计.md)。
> 本目录是契约的**机器可读/样例形态**，与 `test/contract` 契约测试、`internal/core` Go 类型三者同源，
> 任何改动必须同步（CI 以契约测试守护）。总原则：**只增不改**（废弃走版本窗口）；
> **版本绑定在 run**；**幂等键贯穿全链路**；错误码稳定。

| 文件 | 内容 |
|---|---|
| [session-api.openapi.yaml](./session-api.openapi.yaml) | Session API（对外 REST + SSE）OpenAPI 3.0 定义 |
| [runs-protocol.md](./runs-protocol.md) | `/runs` 协议（worker ↔ harness 唯一协议）+ 请求/SSE 帧样例 |
| [executor-protocol.md](./executor-protocol.md) | Executor 协议（worker ↔ executor）+ 生命周期与幂等语义 |
| [events.schema.json](./events.schema.json) | 事件 schema（append-only，类型全集枚举） |

样例数据（Go 契约测试读取）：

- `../../test/contract/testdata/runs.request.json` — /runs 请求样例
- `../../test/contract/testdata/runs.frames.ndjson` — SSE 帧序列样例（含截断 done）
- `../../test/contract/testdata/events.sample.json` — 事件行样例
