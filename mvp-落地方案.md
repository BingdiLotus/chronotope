# MVP 落地方案（v3：Go 平台 + Python harness + Restate）

> 项目：**Chronotope（时空可组合持久运行时）** · 命名规范见 [README](./README.md)
> 上游：[agent-saas-持久执行调研报告.md](./agent-saas-持久执行调研报告.md) · [mvp-技术选型.md](./mvp-技术选型.md) · [mvp-架构设计.md](./mvp-架构设计.md)
> 范围：4 周 × 2–3 人，交付「创建 Agent → 持久 Session → 提交任务 → 崩溃恢复 → 定时/事件唤醒 → 沙箱执行 → 时间轴回看 → 计量」核心闭环。

## 0. 落地三原则

1. **契约先行**：第一周前两天把三个契约（Session API / `/runs` 协议 / Executor 协议 + 事件 schema）定死落库，再写实现——契约即边界，边界即可组合性。
2. **每周一个可演示的增量**：任何一周的产出都必须是可运行、可验收的闭环，不允许「最后一周集成」。
3. **纪律即架构**：harness 无状态、事件 append-only、重放不重调模型/沙箱——这三条违反任何一条，当周验收即失败。

## 1. 仓库结构与模块划分

```
chronotope/                     # 仓库/Go module：github.com/<org>/chronotope
├─ go.work                      # Go workspace（api/worker/executor 三模块）
├─ cmd/
│  ├─ api/                      # HTTP/SSE 网关
│  ├─ worker/                   # Restate 服务端点（核心）
│  └─ executor/                 # 沙箱编排服务
├─ internal/
│  ├─ core/                     # 共享契约：事件 schema、Executor 协议、Session API 类型
│  ├─ store/                    # Postgres 访问层（events/sessions/usage/outbox）
│  ├─ events/                   # 事件投影：SSE hub、outbox 投递
│  ├─ restate/                  # Restate 客户端封装、journal 缓存键 helper
│  └─ execproto/                # execute 协议：docker driver / e2b driver 接口
├─ harness/                     # Python 服务（uv + FastAPI + OpenAI Agents SDK）
│  └─ app/main.py               # POST /runs → SSE
├─ web/                         # Next.js 控制台（W4）
├─ deploy/
│  ├─ docker-compose.yml
│  └─ Dockerfile.{api,worker,executor,harness,web}
├─ docs/contracts/              # 三个契约的正式定义（API-first）
└─ test/chaos/                  # kill -9 恢复验证脚本
```

**命名规范**：二进制 `chronotope-api / chronotope-worker / chronotope-executor`（Go）+ `chronotope-harness`（Python）；镜像 `chronotope/*`；工作目录中文名保留为文档副标题。

**分工建议**：工程师 A（Go 平台：api + worker；**executor 骨架并入 W2 再接**）；工程师 B（harness + 契约 + chaos 测试 + W4 控制台；**W1 后半周转共担事件通路与契约测试**）。事件 schema 两人共同评审。

## 2. 三个先焊死的契约（W1 D1–D2 落库）

> 完整定义见 [契约规范.md](./契约规范.md)（本节为摘要）；边界语义（熔断/权限/截断/删除/时钟/准入）见 [边界语义设计.md](./边界语义设计.md)。

### 2.1 Session API（对外 REST + SSE）

```
POST   /orgs/:orgId/agents           创建 Agent（model/instructions/tools/mcp，version）
POST   /agents/:agentId/sessions     创建 Session → status=created
POST   /sessions/:id/runs            提交任务 {input, trigger}，支持幂等键 → run_id
GET    /sessions/:id/events?after=seq   SSE 时间轴（断线重连按 seq 续读）
POST   /sessions/:id/actions         {action: pause|resume|wake|steer}
GET    /sessions/:id                 Session 状态 + 最近事件
```

### 2.2 事件 schema（`events` 表，append-only）

```json
{ "session_id": "s_1", "run_id": "r_1", "seq": 42, "type": "sandbox.exec",
  "payload": { "tool": "bash", "input": "pytest", "exit": 0 }, "at": "..." }
```

类型枚举（MVP 全集）：`run.started` · `llm.call` · `tool.call` · `sandbox.exec` · `step.journaled` · `run.paused` · `run.awaiting_approval` · `run.resumed` · `run.completed` · `run.failed` · `session.woken`

### 2.3 `/runs` 协议（Go worker ↔ Python harness，唯一协议）

```
POST /runs  {run_id, session_id, step, messages[], tools[], model}
  → SSE 事件流: {type: "delta"|"tool_call"|"done"|"error", payload}
```

- harness **无状态**：不持久化任何会话数据；每次调用全部上下文来自入参。
- 工具分流：API 类工具在 harness 内执行；`code/bash` 类工具 harness 只返回 tool_call，由 worker 转 executor。
- 幂等：相同 `(run_id, step)` 的输出由 **Restate journal 缓存**负责，harness 不需要实现缓存。

### 2.4 Executor 协议（worker ↔ executor）

```
POST /sandboxes      {image, limits, ttl} → sandbox_id     创建沙箱
POST /execute        {sandbox_id, name, input, ttl} → 流式日志 + 结果
PUT/GET /files/{sandbox_id}/{path}                         文件读写
DELETE /sandboxes/{id}                                     回收
```

- 实现：`docker` driver（dev，受限容器）+ `e2b` driver（prod，W4 在 Linux KVM 主机验证切流）。
- capability 字段（`cpu/gpu/network/browser`）从第一天就在协议里，实现可后补。

## 3. Restate 服务与 journal 设计（核心难点拆解）

| Restate 服务 | 类型 | 职责 |
|---|---|---|
| `session_object` | Virtual Object（key=session_id） | 会话状态机：phase、plan、记忆游标、last_run_id；`wake()/pause()/resume()` |
| `run_workflow` | Workflow（key=run_id） | 任务编排：agent loop，每步 journal，模型输出/沙箱结果缓存 |
| `scheduler` | Service | cron 表 → durable timer → `session_object.wake()` |
| `webhook` | Service | `POST /webhooks/approval/{run_id}` → resolve awakeable |

**run_workflow 的步骤形态（Go helper `RunStep`）**：

```go
// 伪代码：每个 RunStep 自动完成「查缓存 → 执行 → journal → 回填缓存」
result := RunStep(ctx, cacheKey(runID, step, model, provider),
    func() (any, error) { return harness.Call(ctx, req) })   // 崩溃重放时直接回放缓存，不重调
```

- **模型输出缓存键**：`run_id + step + provider + model 版本`（模型升级后旧会话用旧缓存跑完）。
- **HITL**：`awakeable := restate.Awakeable()` → journal 挂起事件 → 审批 webhook 到达 → `awakeable.Resolve()` → run 从 Restate 状态恢复继续。**挂起期间零进程占用**。
- **沙箱执行同理**：`call_executor` 也是 journaled side effect，重放不重跑沙箱。

## 4. 里程碑与任务拆解（4 周）

### W1 · 骨架 + 契约 + 最小闭环（纯对话）

| 天 | 任务 | 产出 |
|---|---|---|
| D1 | **Spike（扩围为 1 天）**：最小 workflow + `kill -9` 恢复；并逐项验证 5 个组合原语——①Run 闭包内分钟级 SSE 长流 ②awakeable 跨 HTTP resolve ③child workflow 调用/await ④endpoint versioning（新 invocation 走 v2、在途留 v1）⑤journal/state 条目大小限制量级。任一阻断 → 切 Temporal Go | spike 结论记录 |
| D1–D2 | compose（postgres/restate/litellm/minio）+ 三个契约落库 `docs/contracts/` | 契约文档 + events 建表 |
| D3–D4 | api 骨架（REST + SSE）、worker 骨架（session_object/run_workflow 空壳 + 直连 harness）、harness 骨架（`/runs` 直通 LiteLLM） | 三服务可起 |
| D5 | 联调 + 验收 | 见下 |

**W1 验收**：建 agent → 建 session → 提交任务 → 对话完成 → **SSE 实时收 delta**（事件最简子集落表：run.started/llm.call/run.completed）；OpenAI 兼容端点与 Claude 各跑通一次。W2 补全事件类型全集与 `after=seq` 断线续读。

### W2 · 沙箱 + 时间轴

- Docker executor driver 落地（受限容器：read-only root、egress 白名单、无 secrets、CPU/内存/TTL 限额）。
- worker 工具分流：`code/bash` 类 tool_call → executor。
- 事件全量落 `events` 表；SSE 支持 `after=seq` 断线续读；文件工件 → MinIO。
- **验收**：agent 写代码 → 沙箱跑测试 → 产物回传；kill 掉 api 后重连，时间轴从断点续读。

### W3 · 持久性三件套（核心周）

- **崩溃恢复**：chaos 脚本（run 执行到第 N 步时 `kill -9` worker/executor/harness 各自及组合）→ 重启后自动续跑，且日志证明「不重调 harness、不重跑沙箱」。
- **定时唤醒**：cron 建 schedule → 会话休眠 → durable timer 到点唤醒 → 执行 → 回睡。
- **HITL**：run 挂起 → `awaiting_approval` 事件 → webhook resolve → 继续执行。
- **验收**：三个 demo 场景各有可重放的脚本与记录。

### W4 · 计量 + 控制台 + 演示

- usage 三轴（活跃秒 / token / 计算秒）从事件流聚合（1min 桶）。
- 控制台最小页：会话列表 / 时间轴回放 / 用量。
- demo 脚本 + README（一条 compose up 复现全部场景）。
- 可选加分：Linux(KVM) 主机 E2B 自托管 driver 切流验证。

### W5 · 分层记忆 + 熔断落地

- **分层记忆 + consolidation**（边界语义设计 §7：buildMessages 组装、摘要/记忆条目、topic 标签）——年度会话的硬需求。
- 边界语义落地：cancel/无进展检测/三级预算熔断、工具风险分级 + class 2 强制审批、`done` 终止帧 + `beat` 心跳 + 截断事件、tombstone 两段式删除、调度时区语义、限流 + 排队 + 双开 409。

### W6 · skill/MCP + 子 Agent

- skill 安装 + MCP 连接（落地方案 §11：worker MCP 管理器、dispatcher 加 `mcp` 分支、沙箱恢复路径）。
- 子 Agent（§13：`spawn_subagent` + child run 管道 + fork 沙箱 + 预算护栏）。

### W7 · 群聊多 Agent

- participants + 任务黑板 + moderator 主持循环 + 人类消息注入（§14）。

### W8 · 存储分层与运营

- 存储分层：PG 分区冷层 parquet 归档 + usage rollup（分区 W2 已预留）。
- 孤儿资源 GC 工作流；灾备恢复演练（PITR + 快照恢复进 chaos 套件）。
- 通知与交付（outbox 接邮件/IM + 交付物清单）；会话导出（标准 tar）。

## 5. 关键实现要点与坑位清单

| 点 | 决策 |
|---|---|
| SSE 分发 | PG `LISTEN/NOTIFY` 只发 `(session_id, seq)` 提示（payload <8KB 限制），api 回查后推送；**心跳 + 客户端定期对账**兜底静默丢失；seq 允许 gap |
| outbox | events 与 outbox **同事务**写入；投递 worker 定时拉取 + 指数退避重试 |
| 重放缓存 | 缓存放 Restate state（非 PG），键含模型版本；PG 只存投影事件 |
| 凭证 | 模型 key 只在 LiteLLM；沙箱注入用一次性 env；事件日志明文零密钥 |
| 幂等 | Session API 层 `Idempotency-Key` → run_id 去重；Restate 幂等键 = run_id |
| 多租户 | org 级 quota（max_sessions/concurrency/ttl）；MVP 单 org 跑通，字段先预留 |
| Go 依赖 | chi + pgx + restate-sdk-go（go 1.23+）；Python：uv + FastAPI + sse-starlette + openai-agents-sdk |
| 模型版本 | agent.config 强制带 version；缓存键引用之——从 W1 就写进契约 |

## 6. 测试与验证策略

- **契约测试**：事件 schema 序列化、缓存键、三协议请求/响应样例（W1 起随契约落库）。
- **集成**：compose 全链路 e2e（每次提交跑）。
- **混沌**：`kill -9` 矩阵（api/worker/executor/harness × 执行前/中/后）+ SSE 断线重连 + Restate 引擎重启。
- **验收清单**：每周末按「每周验收」逐条打勾，不满足即顺延下周，不让欠账跨周累积。

## 7. 风险登记表

| 风险 | 概率 | 缓解 |
|---|---|---|
| Restate Go SDK 有坑/不熟 | 中 | W1 D1 spike 半天定生死；降级路径 Temporal Go（边界已隔离） |
| harness 无状态纪律被破坏 | 中 | 契约评审 + CI 检查（harness 代码禁 import 本地存储） |
| SSE 断线丢事件 | 低 | `after=seq` 续读，事件是唯一真相，UI 只是投影 |
| LiteLLM 单点故障 | 低 | 无状态代理重启即愈；用量兜底从事件侧聚合 |
| Docker executor 隔离不足 | 中 | 只跑可信任务；不可信代码路由 E2B driver（capability 路由） |
| 双语言 CI 复杂 | 低 | 三 Go 二进制 + 一 Python 镜像，无 gRPC，仅 HTTP/SSE |

## 8. Definition of Done（整体）

1. 一条 `docker compose up` 复现全部场景（README 记录步骤）；
2. 三个持久性 demo（崩溃恢复 / 定时唤醒 / HITL）有可重放脚本；
3. 时间轴可回看任意 session 的完整事件序列；
4. 计量页展示三轴用量；
5. 契约文档与实现一致（契约测试通过）；
6. 边界语义有契约测试覆盖：终止帧/截断、删除 tombstone、限流/双开 409、熔断与无进展检测。

---

## 9. 附：harness 实现细则与 executor 交互

### 9.1 分工铁律

**harness 不拥有 loop，loop 在 Go worker 的 run_workflow（持久层）。** harness 是「单段执行器」：每次 `/runs` 调用跑一段 agent 循环（模型调用 + API 工具内联执行），遇到代码类工具即停下交棒。原因：崩溃恢复粒度 = 一个 journaled step = 一次 harness 调用；loop 若在 harness 内，崩溃后只能整段重跑，违反「重放不重调模型/沙箱」。

### 9.2 harness 实现要点（Python ~200 行，无状态）

- 无 Session、无本地存储；消息历史/工具 schema/模型名全部来自入参（OpenAI Agents SDK 的 Session 特性故意不用）。
- 模型调用永远走 LiteLLM（OpenAI 协议）；「借」的是 openai SDK 的工具调用/流式 + LiteLLM 的多模型翻译。
- 工具分流：`API_TOOLS`（web_search/http_request 等）内联执行并继续循环；`CODE_TOOLS`（bash/run_python/read_file/write_file）发出 `tool_call` 事件后返回。
- 三种返回：`done`（终答）/ `tool_call`（交棒）/ `error`（max_turns）。
- API 类工具编排复杂化后，可换 OpenAI Agents SDK 的 Runner（仅跑内联工具段），`/runs` 协议不变。

### 9.3 executor 交互（harness 从不直连 executor）

1. worker 收到 `tool_call` 事件 → 按 session 的 sandbox_id（懒创建、session 作用域）调 executor `POST /execute`；
2. executor 流式回传日志 + `{exit, output}`；
3. worker journal 结果（RunStep 缓存）→ 追加 tool_result 消息 → step+1 → 再次调 harness。

理由：journal 集中一处（重放语义单一）、harness 保持零状态（SSE 单向，无法挂起等回调）、文件走 `/files` 快路径（不经过 shell）。

### 9.4 崩溃矩阵

| 挂掉谁 | 行为 |
|---|---|
| harness | worker 按 Restate 重试策略重发同一 `(run_id, step)` 的 `/runs`（幂等） |
| executor | 重发 `execute`；沙箱按 sandbox_id 恢复或重建 |
| worker | Restate 重放到断点：已完成 step 读缓存，断点 step 继续 |

---

## 10. 端到端示例：编写 GBA 模拟器（用户指令 → 交付）

**指令**：「写一个能在 macOS 上运行的 GBA 模拟器，支持经典测试 ROM 验证，完成后给我可执行文件和源码。」

| 阶段 | 内容 |
|---|---|
| 0 准备 | 建 Agent（工具 `[bash, write_file, read_file, web_search, request_approval]`，沙箱 `golang:1.23-bookworm`）→ 建 Session |
| 1 提交 | `POST /sessions/:id/runs` → `run.started` → worker 启动 run_workflow |
| 2 主循环 | 规划+`write_file` 骨架（沙箱懒创建）→ `bash: go build` 报错 → 错误作为 tool_result 回喂 → 修复 → 循环数百 step；文件累积在会话作用域沙箱 |
| 3 崩溃恢复 | step 47 时 kill -9 worker → Restate 重放：step 1–46 走缓存（零模型调用/零沙箱执行），仅未完成的 step 47 重发一次 |
| 4 HITL | `request_approval` 控制工具 → awakeable 挂起（零进程占用）→ 控制台批准 → webhook resolve → 继续 |
| 5 交付 | 最终 build → 产物经 `/files` 取出 → MinIO → `run.completed {artifacts}` → 控制台时间轴+下载+三轴计量 |

**事件节选**：`run.started` → `llm.call{step, tokens}` → `tool.call{write_file}` → `sandbox.exec{go build, exit:2}` → … → `run.awaiting_approval` → `run.resumed` → `run.completed{artifacts}`。

**呼应卖点**：任务跨天 = session 休眠 + cron 定时 wake 从持久状态续写；换环境 = capability 路由切 executor driver。**MVP 边界**：不做上下文压缩（长任务靠分阶段+休眠唤醒），压缩为后置能力。

---

## 11. 交付后迭代：安装 Skill/MCP 并重构（会话延续 + 能力注入）

**落点**：Skill = 文件（沙箱 `skills/<name>/`，session 作用域，模型经 read_file 使用）；MCP = 连接（**worker 托管客户端**，harness 仍只见 schema、零状态）。工具分发扩为四类：API 工具（harness 内联）/ 代码工具（executor）/ 控制工具（worker awakeable）/ **MCP 工具（worker MCP 客户端）**。

**流程**：
1. `POST /sessions/:id/skills` → worker 经 `/files` 写入沙箱 + 指令 patch → 事件 `skill.install`
2. `POST /sessions/:id/mcp` → worker 建连 → `tools/list` 缓存进 session 状态 → 事件 `mcp.connected`（stdio 型 MCP 进程跑在沙箱内，安全边界一致）
3. 沙箱存活检查：已回收则从 MinIO 按 session 快照恢复文件——「一周前会话今天还能继续」的机制
4. 新 run（缓存键与旧 run 隔离）→ 循环：read_file(SKILL.md) → MCP 工具调用（事件 `mcp.call`）→ 重构 + build/test → 崩溃恢复/HITL/交付全部复用

**MVP 增量（+1~1.5 周）**：worker MCP 客户端管理（中）、dispatcher 加 mcp 分支 + `mcp.call` 事件（小）、skill 安装即文件操作（小）、沙箱恢复路径（中）。**后置**：skill 市场/版本化、MCP 权限审批流、MCP 资源协议。

---

## 12. executor 资源利用率设计

**本质**：活跃计算秒 ≪ 沙箱墙钟秒（LLM 思考占大头）。优化主轴 = 让闲置沙箱尽快让出资源、需要时秒级还回。

**四级生命周期（核心机制，与休眠/唤醒同构）**：0 运行 → 1 冻结（`docker pause`/freezer，~60s 无 exec 触发，毫秒恢复，释放 CPU）→ 2 快照（CRIU/Firecracker snapshot 落盘，1–10s 恢复，释放内存）→ 3 拆除（TTL/会话结束，文件已增量同步 MinIO，10–60s 恢复）。worker 可发 `step_done` 提示加速冻结倒计时。

**装箱**：内存硬约束不超卖、CPU 可超卖；right-size 默认小规格按需扩容；KSM 同页合并（同镜像 N 沙箱省 10–30% 内存）；warm pool 预启 + 缓存镜像（冷启动 <125ms→近 0，池按需求预测）；session 亲和固定宿主，满载先降级再迁移。

**执行效率**：持久 shell 复用、文件快路径、cgroup 弹性 CPU + 硬内存 cap、按镜像类共享构建缓存卷。

**计量**：utilization = 活跃秒/墙钟秒 为北极星指标；计费按计算秒与利用率激励对齐。

**落地优先级**：W2（懒创建 + Tier1 冻结 + TTL 拆除 + MinIO 快照恢复 + right-size + 镜像预拉）→ W4（利用率面板）→ 后置（KSM/CRIU/装箱调度/多宿主/warm pool/GPU MIG）。

---

## 13. 子 Agent 活动设计

**核心**：子 Agent = 一个子 run（run_workflow 递归复用，可组合主张的兑现）。无新组件，dispatcher 加一类工具。

| 维度 | 设计 |
|---|---|
| 身份 | 独立 run_id → 独立 step/journal/缓存键/事件流；可用更便宜的模型配置 |
| 触发 | 控制工具 `spawn_subagent(spec, input)`（不走 harness 内部 handoff，委派必须进平台视野） |
| 空间 | **独立沙箱，从父快照 fork（写时复制）**；产物经 MinIO 引用回传 |
| 上下文 | 子只拿任务+相关上下文，不继承父历史；父只收结构结果摘要 |
| 并行 | Restate child workflow 一等公民：父派发 N 个子 run + durable await，结果 journal |
| 限额 | max_depth≤2、max_concurrent≤4、每 run 成本预算（spawn 时校验，超预算转审批） |

**崩溃语义（递归持久性）**：子进程挂 → 子独立重放，父在 await 等待；父挂 → 重放到派发点，child workflow 调用在父 journal 里（已完则取缓存、未完则继续等），不重复派发。

**复杂点**：① 合并冲突——子 Agent 只读工作区+返回 diff，父统一落盘（单写者延续到文件级）；② 并行 token 乘法——预算护栏必配。

**增量（约 +1 周，W5）**：spawn 工具+分支（小）、子 run 管道（中，原语现成）、fork 沙箱（中，复用 Tier2 快照）、树形时间轴（小–中）、限额（小）。**后置**：A2A 跨平台委派、自动拆分计划器。

---

## 14. 群聊多 Agent 设计（水平黑板拓扑）

**与子 Agent 的区别**：子 Agent = 树（父派生子、互不可见）；群聊 = 黑板 + 主持路由（同侪共享日志、任务黑板，moderator 决策发言顺序）。底层同用 child run。

| 群聊概念 | 架构映射 |
|---|---|
| 群聊空间 | session + `participants[{agent_config, role}]` + 共享消息日志 + 任务黑板 + 共享沙箱 |
| 主持协调 | `group_run` workflow：moderator harness 调用（「下一位谁发言」），**发言决策 journaled** |
| 成员发言 | child run（参与者 config），durable await |
| 人类插话 | `POST /sessions/:id/messages` 入队 → 下一轮 moderator 可见 |
| 共享工作区 | 一个共享沙箱 + git 分支；任务卡片单写（单写者延续到文件级） |

**持久性**：成员进程挂 → 该 child run 独立重放；协调进程挂 → 重放到断点，发言决策已 journal、不重新决策不重复派发；全员重启 → 黑板/日志/发言各自恢复。

**预警**：① moderator 乒乓（max_turns + prompt 纪律兜底）；② 共享沙箱写冲突（任务卡片单写 + git 分支）；③ 上下文膨胀最快（窗口 = 角色指令 + 最近 N 条 + 黑板快照，完整压缩后置）。

**增量（约 1~1.5 周，W5 并列）**：participants+黑板（小）、主持循环（中）、人类消息注入（小）、git 分支+卡片单写（中）、发言者归属时间轴（小）。**后置**：A2A 去中心化、自动组队、冲突解决 agent。

---

## 15. 换模型与换 harness 的升级语义

**两个接缝 + 一个原则**：LiteLLM 网关 = 模型缝；`/runs` 协议 = harness 缝；**版本绑定在 run 上**（run 启动时绑定 model 版本 + 协议版本，重放缓存键含版本，换装永不污染旧 journal）。

**换模型**：改 agent config + LiteLLM 映射，0 代码；在途会话按启动绑定跑完，新 run 生效；厂商故障走网关 fallback 链；AB 测 = 两个 config 版本并行。

**换 harness 三层级**：A 内部升级（换镜像，0 改动）；B 换框架实现（按 `/runs` 契约重写，契约测试必过，0 平台改动）；C 换托管 harness（写 adapter，边界重划：loop 移交对方，事件日志/自托管沙箱/计量仍是我们的，loop 级崩溃恢复依赖对方 SLA）。

**隐藏纪律**：契约钉死**工具名词汇表**（bash/write_file/request_approval/spawn_subagent…），任何 harness 实现必须发规范名，否则可替换性失效。

**蓝绿与回滚**：新旧 harness 并行、按 run 绑定协议版本路由；回滚 = 改回 config 版本/镜像 tag，缓存键含版本不破坏重放。**安全网**：golden 契约测试（MVP 必建），完整 eval 套件后置但必排。

**定位**：薄 loop（几百行）+ 接缝制 = 前沿特性（压缩/computer use/MCP 演进/A2A）都落到既有接缝增量吸收，无需整体重写。

---

## 16. 年度级会话审计（单用户 × 每 Agent 单会话 × 多年）

**结论**：持久性机制（journal/版本绑定/重放读缓存）天然扛住；但「上下文压缩/分层记忆」与「存储分层」从后置升级为**硬需求**。

| 维度 | 判定 | 对策 |
|---|---|---|
| 崩溃恢复/版本漂移 | ✅ | 年老 run 重放只读缓存，**不需要当年的 harness 镜像与模型**（运维负担收敛到常数） |
| Restate 会话状态 | ✅ | 只存协调态（游标/引用/配置），大载荷在 PG/MinIO（CI 强制纪律） |
| 每步上下文成本 | ❌ | **分层记忆**：工作记忆（run 级窗口）+ 主题记忆（滚动摘要）+ 长期记忆（pgvector 检索 + 文件记忆）；`buildMessages` 组装而非全量携带；事件日志仍是真相，摘要可重算 |
| 消化（consolidation） | ❌ | run 结束后离线 workflow 提炼摘要/记忆（Anthropic Dreaming 模式），journaled、版本化 |
| 事件/消息存储 | ⚠️ | 热层 PG 按 session 分区（近 90 天）→ 冷层对象存储（全量）→ rollup（usage 1min→小时→天）；沙箱快照保留 N 份 + git 归档 |
| 主题漂移 | ⚠️ | topic 标签（模型分类+用户可选）+ 检索按 topic 作用域 + 黑板按主题分区；会话保持物理唯一 |
| 合规 | ✅ | 事件按 session 键隔离，删除/导出/法定保留可按键处理 |

**排期调整**：分层记忆 + consolidation 升级为 W5 硬需求（与 skill/MCP/子 Agent/群聊并列）；PG 分区从 W2 建表预留；存储分层 W6+；主题标签随分层记忆轻量起步。

---

## 17. 运行中升级（平台发版 × 在途 run）

**定义**：平台升级 = 精心组织的「全员崩溃」，按崩溃恢复处理。真正难题只有一个：worker workflow 代码变更必须与在途 journal 重放兼容。

**重放兼容纪律**：step 名/事件类型是持久契约只增不改；状态 schema 向后兼容（加字段带默认值、不删字段）；循环控制流变更用「新代码含旧分支（按 run 绑定版本 switch）」。

**三道防线**：① 版本绑定在 run（在途 run 在旧实例 drain 完，新 run 走新版本）；② Restate endpoint versioning（v2 端点接管新 invocation，v1 在途原地跑完）；③ 代码纪律纵深防御（v1 意外死亡 v2 也能重放 v1 journal）。

**逐组件**：api 滚动（SSE `after=seq` 续读，用户只见一次重连）；worker 双版本+drain；executor 滚动（沙箱事实状态在 docker/PG，不随进程死）；harness 蓝绿（按 run 协议版本路由）；Restate/PG/MinIO 滚动升级；LiteLLM 重启即愈（断流 step 幂等重发）。

**Runbook**：预检（在途 run 清单+绑定版本+快照）→ 基础设施滚动 → harness 蓝绿 → executor 滚动 → worker 双版本 drain → api 滚动 → 全程注入 kill -9 演练 → 回滚 = 改回 tag（缓存键含版本不破坏重放）。

**边界**：天级长 run × 每周升级 = 旧实例多活几天（无状态、便宜）或 run 迁移工具；bug 必须中途生效 = 兼容改法靠重放自然生效，否则 run 迁移；HITL 挂起/休眠会话升级零风险（awakeable/状态在 Restate）；PG schema 用 expand-contract；Restate 大版本走官方数据迁移。

**纪律落地**：golden journal 重放测试进 CI，升级永远常规化。

---

## 18. 未覆盖场景清单（审计补充）

**A 类·设计缝（需补）**
1. **Agent 失控与成本熔断**：无进展检测（同状态重复 N 轮终止）+ 三级熔断（租户/用户/全局）+ Restate cancel（保留已产出文件）。
2. **欠费冻结**：信用耗尽 → awakeable 挂起（零成本）+ 充值 resolve——挂起机制的产品化。
3. **Prompt injection 与工具权限**：沙箱文件/下载内容可含注入指令；MCP 工具 allowlist；敏感操作强制 `request_approval`；工具调用审计。
4. **流截断与超大输出**：`done` 终止帧 + 幂等重发 + 超时；输出限额 + 截断事件（截断也是 journaled 事实）。
5. **数据灾难恢复**：PITR + 恢复演练进 chaos 周；事件日志异地复制留位；快照丢失降级为重构建。
6. **删除权 vs append-only**：tombstone + 物理 compaction（按 session 键）；审批事件即审计轨迹。
7. **多租户隔离边角**：skill/MCP 租户隔离、共享缓存/KSM 侧信道、沙箱逃逸爆炸半径（网络分区+命名空间）。
8. **通知与交付**：outbox 接邮件/IM/推送 + 交付物清单；会话分享/协作权限模型。
9. **时区与时钟**：cron 时区/夏令时语义、NTP 回拨对 timer 的影响（契约写明、测试覆盖）。
10. **生态互操作**：A2A 入站、会话作为 MCP server 暴露、OpenAI Agents API 兼容层（商业分发策略）。
11. **孤儿资源 GC**：崩后容器/快照/awakeable/TTL 失效沙箱的周期回收工作流。
12. **提交洪峰与同会话双开**：api 限流+排队语义；双开 = 409 + 排队提示（明确定义）。

**B 类·已知后置**：多区域灾备、免费 tier 滥用、会话全文搜索、eval 回归 CI（换模型必跑）、模型供应商数据条款（ZDR/DPA，售前问题）。

**优先级**：1/3/4/6/9/12 现在补设计（契约层零成本，晚补要改协议）；5/11 并入 W3 chaos；2/7/8 排 W5+；10 与 B 类属商业决策。

**状态更新**：1/3/4/6/9/12 的设计已补齐 → 见 [契约规范.md](./契约规范.md)（事件类型全集、错误码、终止帧、删除语义）与 [边界语义设计.md](./边界语义设计.md)（熔断/权限分级/截断/留存/时钟/准入/分层记忆）；W5–W8 里程碑已纳入 §4（审核后重排）。
