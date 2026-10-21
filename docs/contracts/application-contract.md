# 应用契约 v1.0.0（Bots 对接面）

> 机器可读定义：[application-contract.v1.json](./application-contract.v1.json) ·
> OpenAPI：[session-api.openapi.yaml](./session-api.openapi.yaml) ·
> 错误/观测：[错误分类与可观测性](../错误分类与可观测性.md) ·
> 动态 target：[动态 target 注册与 fencing](../动态-target-注册与-fencing.md)

本文固定 Bots 与 Chronotope 之间的**应用契约**：agent/session/run、application
tools bridge、timer create/fire、tenant 归属、幂等键和错误响应。它不覆盖真实
Slack 发送、Bots 业务对象或 Bots 产品验收。

## 1. 版本与兼容边界

| 项 | 值 |
|---|---|
| 应用契约版本 | `1.0.0`（SemVer） |
| 响应头 | `X-Chronotope-Application-Contract: 1.0.0` |
| run/harness 协议 | `1.0`（独立版本，见 [runs-protocol](./runs-protocol.md)） |
| 机器契约 | `docs/contracts/application-contract.v1.json` |
| Bots 最低已兼容版本 | `v0.1.0-mvp`（既有 HTTP 形状兼容） |
| 使用稳定错误码所需 Bots 改动 | 下一 patch：读取 `error_code`/`category`/`retryable`（见 §5） |

兼容规则：

1. **同 major 只做增量**：新增可选字段、新 error code、新日志/指标标签值；
   调用方必须忽略未知字段和未知 error code。
2. **破坏性变更升 major**：字段改名/删除、HTTP 状态语义变化、幂等键语义变化、
   bridge/timer 信封形状变化。升级必须给出 Bots 同步窗口和迁移说明。
3. `error` 文本只用于诊断，禁止作为分支依据；分支只使用 `error_code`、
   `category`、`retryable` 和 HTTP status。
4. run 绑定协议/配置版本，并记录 `application_contract_version`；旧 run 不因
   新契约部署而改变含义。历史 fixture 只证明其覆盖的版本，不代表跨版本接管。
5. 当前没有生产兼容层承诺；本文件声明的 `1.x` 增量兼容是 P0 起的门禁。

## 2. 错误信封

所有 HTTP 错误（包括认证中间件失败）返回：

```json
{
  "code": 409,
  "error_code": "idempotency_conflict",
  "error": "idempotency key ...",
  "category": "conflict",
  "retryable": false
}
```

- `code` 保留旧字段（HTTP status）。
- `error_code` 是稳定机器码，完整集合以 JSON fixture 为准。
- `category ∈ {validation, auth, not_found, conflict, rate_limit, provider,
  tool, timer, runtime, limit, policy, canceled}`。
- `retryable` 只表示**同一逻辑操作在稳定幂等键下**是否可安全重试；不表示 run
  可恢复，也不表示外部效果未发生。
- run 终态 `GET /runs/{id}` 的 `error` 同样带 `code/message/category/retryable`。
- timer 查询新增 `last_error_code`；`state=blocked|retrying` 时与
  `last_error` 同时存在。

## 3. 契约覆盖矩阵（无真实 Slack/Bots）

| 契约面 | 断言 | 所在测试 |
|---|---|---|
| 契约版本/fixture/错误码同步 | fixture 与 Go catalog 逐项一致、OpenAPI 声明版本 | `test/contract/application_contract_test.go` |
| agent/session/run 创建 | 201/200 幂等、原 ID、subject/scope 冻结 | `internal/api/application_contract_test.go` |
| tenant 归属 | 跨租户 agent/run/timer 403 `tenant_mismatch`；跨 scope 404 | 同上 + `internal/api/timers_test.go` |
| 幂等/忙碌 | 同键异请求 409、`session_busy` + `active_run_id`、同键重试原 run | 同上 |
| 应用工具 bridge | 信封 context、`Idempotency-Key=operation_id`、Bearer、2xx/未知结果；样例 fixture 同源 | `internal/restate/application_bridge_contract_test.go`、`test/contract/testdata/application.bridge-call.json` + `fakeapp` |
| timer create/fire | 202 + Location、Fire 形状、`Idempotency-Key=fire_id`、稳定 fire_id、ACK；样例 fixture 同源 | `internal/api/timers_test.go`、`internal/restate/timer_test.go`、`test/contract/testdata/application.timer-fire.json`、`fakeapp` |
| timer fencing | target 改址/撤销后旧 timer blocked，不发送新地址 | `internal/restate/timer_test.go`、`internal/targets/registry_test.go` |
| 动态 target | 注册/更新/撤销、static ref 防遮蔽、bridge 路由不可变、tenant/scope 唯一 | `internal/api/application_targets_test.go`、`internal/store/application_targets_test.go` |
| 错误分类 | provider/tool/validation/limit 稳定分类，effects 后不自动重试 | `internal/restate/application_tools_test.go` |
| 观测 | 指标 exposition、错误码有界标签、无凭据 | `internal/observability/metrics_test.go` |

`test/contract/fakeapp` 是 **Bots-like application receiver** 假实现：它不导入
Chronotope 的生产 API 类型，独立校验 bridge/timer 请求形状、认证和幂等头。它
不是 Bots 产品代码，也不证明 Slack 最终送达。

## 4. 可重复运行

```bash
# 契约与 Go 全量测试（store 集成测试需要隔离 PG；CI 用 service container）
go test ./test/contract/... ./internal/api/... ./internal/restate/... ./internal/targets/...

# 本地隔离 PG（不要指向开发库）
docker run --rm -d --name chronotope-p0-test-postgres \
  -e POSTGRES_DB=chronotope_test -e POSTGRES_USER=chronotope \
  -e POSTGRES_PASSWORD=chronotope_dev -p 127.0.0.1:55432:5432 pgvector/pgvector:pg16
STORE_TEST_DATABASE_URL='postgres://chronotope:chronotope_dev@127.0.0.1:55432/chronotope_test?sslmode=disable' \
  go test ./...
```

契约测试不启动真实 Slack、Bots 或真实模型；`test/fixtures/application-sink.py`
仍用于 C3 全栈 fake 门禁，与本契约测试互补。

## 5. Bots 最小升级步骤

不改 Chronotope 代码即可保持 `v0.1.0-mvp` 的请求/响应兼容。要获得稳定错误
分支，Bots 客户端最小改动：

1. 保留现有 `Idempotency-Key`/`origin_command_id`/operation ID 生成；
2. 解析错误 JSON 的 `error_code`、`category`、`retryable`，未知 code 按
   `category` 或 HTTP status 降级；
3. 409 `idempotency_conflict` 不换 key；409 `session_busy` 持久排队后用
   **同一 key** 重试；429/5xx 指数退避 + 同一 key；
4. run 失败读取 `error.category/retryable`；只有 `retryable=true` 且本地
   operation 账本确认无外部效果时才可重新提交同一逻辑操作；
5. `application_tool_result_unknown` 不得自动重做，按 `operation_id` 对账；
6. timer `state=blocked` + `last_error_code` 视为终止/人工处理，不重新计时
   绕过 fencing；
7. 记录响应头契约版本；主版本不匹配时拒绝静默继续。

## 6. 明确不在本契约内

- Bots 的 list_tasks/list_reminders、memory 查询/forget（Bots 应用工具/PG，
  Chronotope 不实现）；
- Slack API 调用、最终送达回执、Bots 业务队列；
- 取消、deadline/budget、tool timeout、HA/PITR、审批/HITL、streaming、
  MCP、multi-agent 等 P1 项。
