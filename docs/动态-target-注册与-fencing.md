# 动态 bridge/timer target 注册与 fencing（P0-2 最小实现）

> 状态：v1 最小实现已落地（`application_targets` / `application_target_audit`，
> migration 040）。静态 env bootstrap 仍是部署基线；动态表是运行时 overlay。
> 不是 HA/缓存/多活方案，也不改变“应用拥有最终验收”的边界。

## 1. 问题与目标

现状：`APPLICATION_BRIDGES_JSON`、`APPLICATION_TIMER_TARGETS_JSON` 在 API/worker
启动时加载；新增 tenant/Bot 或改接收地址必须重启。target 变化后旧 timer 会看到
“configured timer target changed or unavailable”，但没有机器可读原因，也容易
被误当成普通可重试错误。

目标：

1. 不重启 API/worker 即可注册/更新/撤销 bridge 与 timer target；
2. 静态 env 继续可用且语义不变；
3. target 有 tenant/ref 唯一性、服务认证、审计；
4. **fencing**：旧 timer 不因 target 更新/撤销而悄悄改投新地址，必须
   `blocked` + 稳定错误码；
5. 覆盖新增/修改/删除、旧 timer、错误 target、跨 tenant 回归；
6. 风险大时先设计+最小实现，不为了“动态”破坏静态部署。

## 2. 数据模型

`application_targets`（migration 040）：

| 列 | 语义 |
|---|---|
| `kind` | `bridge` 或 `timer` |
| `ref` | 全局唯一引用；agent 的 `application_bridge_ref` / timer 行 `target_ref` |
| `tenant_id` | 归属租户（FK orgs） |
| `url` | bridge URL 或 timer callback URL；不接受 userinfo/fragment |
| `subject_ref/scope_id` | timer 必填；bridge 必须为空 |
| `token_env` | 服务凭据的**环境变量名**；不存 token 值 |
| `revision` | 变更递增 |
| `state` | `active` / `revoked`（软撤销，ref+route 继续保留） |
| `digest` | 路由摘要（ref/tenant/url/subject/scope）；credential 轮换不变 |

`application_target_audit(kind, ref, tenant_id, action, actor, revision, digest,
created_at)` 与 target 变更同事务写入。

约束：

- `PRIMARY KEY(kind, ref)`：ref 全局唯一；
- 部分唯一索引 `(tenant_id, subject_ref, scope_id) WHERE kind='timer' AND state='active'`：
  一个租户/主体/scope 只能有一个 active timer receiver；
- bridge 允许同租户多个 ref（不同 Bot / 不同 bridge）；
- 软撤销而非硬删除：防止删除后同名 ref 被重新登记成新地址，悄悄改变旧
  agent/timer 的接收方。

## 3. 注册/更新/撤销 API

仅平台 operator（`API_AUTH_MODE=on` 时要求 `API_ADMIN_KEY`；auth off 是显式开发
模式）。tenant key 可以创建/查询业务资源，但**不能**改回调接收方。

```http
POST /orgs/{orgID}/application-targets
Idempotency-Key 不要求（更新语义由 revision 表达）
Content-Type: application/json

{
  "kind": "timer",
  "ref": "bots-timers-slack-g1",
  "url": "http://bots.internal/internal/timer-fires",
  "subject_ref": "bot_1",
  "scope_id": "scope_1",
  "token_env": "APPLICATION_SERVICE_TOKEN"
}
```

- 首次返回 `201`；同 ref 再次提交返回 `200`，`revision+1`；
- `token_env` 必须已存在于 API 进程环境且非空；不接受内联 token；
- bridge 的 `tenant_id/ref/url` 是**不可变路由**；只允许轮换 `token_env`。
  换地址必须登记新 ref，并显式创建使用新 ref 的 agent；
- timer 的 address/subject/scope 可更新，但 digest 变化会使旧 timer 在回调前
  `blocked`；
- ref 与静态 env 冲突返回 409 `target_ref_static`；不得动态遮蔽静态配置；
- 跨 tenant 的 delete/list 不可见（404 / 空列表语义），operator 也必须显式
  指定 org 路径。

```http
GET    /orgs/{orgID}/application-targets
DELETE /orgs/{orgID}/application-targets/{kind}/{ref}
```

DELETE 是软撤销：`state=revoked`、`revision+1`、写审计；不提供“恢复”接口，
恢复 = 同 ref 同 route 重新 POST。

响应/列表包含 `url`、`token_env`、`revision`、`state`、`digest`，**不包含 token
值**。列表只返回动态 overlay；静态 env target 仍以部署配置为唯一真相。

## 4. 解析语义（API 与 worker 共享）

`internal/targets.Registry` 合并：

1. 启动时 env bootstrap（`appconfig.Load`，原有校验不变）；
2. 每次使用时按 tenant/ref 查询 `application_targets`（当前**不缓存**，优先
   正确性；后续可加短 TTL 缓存 + 失效通知）。

规则：

- **static/dynamic 同 ref**：fail closed（`ErrStaticTargetConflict`），不静默
  选择一方；动态注册 API 提前拒绝这种 ref。
- **bridge 解析**：每次 tool call 前解析 `application_bridge_ref`，校验
  `tenant_id` 与 run bound tenant 一致，Bearer 从 `token_env` 当前值读取，
  不跟随重定向。旧 run 的 ref 不变；若 target 被撤销，tool call 失败并终止，
  不会改投。
- **timer 创建**：按 `tenant_id/subject_ref/scope_id` 合并 static+dynamic，
  必须恰好一个候选；0 个 → 403 `timer_target_unavailable`，多个 → 503
  `service_unavailable`。选中 target 的 `ref+digest` 冻结进 timer 行。
- **timer fire**：按 timer 行的 `tenant_id/target_ref` 重新解析当前 target：
  - 不存在/revoked/token 缺失/URL 非法 → `blocked`，
    `last_error_code=timer_target_unavailable`；
  - 存在但 digest 与 timer 行冻结值不同 → `blocked`，
    `last_error_code=timer_target_changed`；
  - digest 相同 → 正常投递；2xx 后 ACK 写回；
  - 任何 blocked 路径都在 HTTP 调用之前返回，旧 timer 不可能改投新地址。
- **credential 轮换**：`token_env` 变化不改变 digest；下一条回调/工具调用使用
  新 token，旧 timer 不因轮换 blocked。这是唯一允许的“静默变化”，因为它不改变
  接收方。

## 5. Fencing 语义表

| 变更 | 新请求 | 旧 timer | 旧 agent 的 tool call |
|---|---|---|---|
| bridge token_env 轮换 | 新 token | 不适用 | 下一次调用用新 token，地址不变 |
| bridge URL/tenant 变更 | 409 `target_route_immutable` | 不适用 | 不可能发生（被拒） |
| bridge 撤销 | 后续 agent 创建/tool call 失败 | 不适用 | 终止，不改投 |
| timer token_env 轮换 | 新 token | digest 不变，继续投递 | 不适用 |
| timer URL/subject/scope 更新 | 新 timer 用新 digest | `blocked` + `timer_target_changed` | 不适用 |
| timer 撤销 | 新 timer 403 | `blocked` + `timer_target_unavailable` | 不适用 |
| static ref 被动态注册 | 409 `target_ref_static` | 解析 fail closed | 解析 fail closed |

`blocked` 是**终止/需人工处理**状态，不是可自动重试状态。应用若选择重新安排
提醒，必须显式使用新 revision/timer，不得要求 Chronotope 悄悄换目标。

## 6. 安全与审计

- URL 必须是绝对 `http(s)`，拒绝 userinfo/fragment；不跟随重定向；
- 只接受 `token_env` 引用；token 值不进入 PG、API 响应、日志、指标；
- API/worker 必须注入同名 `token_env`；缺失时 fail closed（不发送无认证请求）；
- 注册/更新/撤销要求 operator 权限；tenant 路径 + DB 查询双重 tenant 校验；
- 每次变更写 `application_target_audit`（actor = principal/admin/dev），并输出
  `target.registered` / `target.revoked` 结构化日志与
  `chronotope_application_target_changes_total` 指标；
- bridge/timer 解析仍校验 run bound tenant/subject/scope，模型参数不能选 URL。

## 7. 向后兼容与部署步骤

1. 静态 env 配置、启动校验、现有 timer `target_digest` 语义保持不变；
2. migration 040 启动时自动执行（API `Migrate`；CI/compose initdb 同源）；
3. API 与 worker 部署同一版本，且都有 `token_env` 指向的凭据；
4. 先部署代码，再按需 `POST` 动态 target；无需重启；
5. 回滚代码不会删除 `application_targets`/audit 表；旧二进制忽略它们，静态
   env 继续工作。若回滚后动态 target 仍被使用，需先撤销或迁移为静态配置。

## 8. 明确限制（P1 / 后续）

- 每次解析 hit PG；没有缓存、watch、多活一致性协议。高频 timer/fire 下需加
  短 TTL 缓存与失效广播（不得牺牲 digest fencing）。
- 没有 UI/CLI、批量导入、审批流；当前用 operator HTTP API。
- 没有自动 target 健康检查/熔断；错误回调按 timer 语义重试/blocked。
- 没有 tenant key 轮换、配额/速率限制（P1）。
- 动态 target 的 `subject_ref/scope_id` 是 opaque 字符串；Chronotope 不解释
  Bot/频道语义。

## 9. 回归证据

```bash
# 无 DB 的解析/API 语义
go test ./internal/targets ./internal/api -run 'TestApplicationTarget|TestDynamicTimer' -count=1

# 真实 PG（隔离库）
STORE_TEST_DATABASE_URL='postgres://chronotope:chronotope_dev@127.0.0.1:55432/chronotope_test?sslmode=disable' \
  go test ./internal/store -run TestApplicationTargetRegistryAndFencing -count=1

# worker 端 fencing（旧 timer 不发送新地址）
go test ./internal/restate -run 'TestDynamicTimer|TestTimerTargetDrift' -count=1
```
