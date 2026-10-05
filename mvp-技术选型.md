# MVP 技术选型建议（v2：Python 主力 · 沙箱自托管 · 多模型）

> 项目名：**Chronotope**（时空可组合持久运行时）
> ⚠️ **§1–§2 为已废弃的 v2（纯 Python）方案**，保留作决策历史。**现行方案为 §8（v3：Go 平台 + 借 harness）**，并以 [契约规范.md](./契约规范.md) 与 [mvp-落地方案.md](./mvp-落地方案.md) 为准。

> 前提文档：[agent-saas-持久执行调研报告.md](./agent-saas-持久执行调研报告.md)
> 已确认的决定：①主力语言 **Python**；②沙箱**必须自托管**；③模型接入 **OpenAI 兼容端点 + Anthropic，支持多模型切换**。

---

## 0. MVP 范围（4–6 周）

跑通核心闭环：**创建 Agent → 建持久 Session → 提交任务 → 崩溃自动恢复 → 定时/事件唤醒 → 沙箱执行代码 → 时间轴回看**。

明确不做：多区域、BYOC 生产化、A2A、Stripe 正式计费（只做计量）、多 Agent 编排、上下文压缩调优。

## 1. 分层选型总表（v2）

| 层 | 选型 | 核心理由 | 备选 |
|---|---|---|---|
| **持久执行核心** | **Restate**（Apache-2.0，单二进制）+ **Python SDK** | Session = Virtual Object；`ctx.sleep` = 跨重启定时器；Awakeable = webhook/HITL 唤醒；**无确定性约束**；官方有 [OpenAI Agents SDK 集成](https://docs.restate.dev/ai/sdk-integrations/openai-agents-sdk)与 [Durable Sessions](https://docs.restate.dev/ai/patterns/sessions) 模式 | **Temporal + Python SDK**（最成熟的保守降级路径：其 ×OpenAI Agents SDK 集成已 GA）；DBOS（TS only，不适用） |
| **模型网关** | **LiteLLM**（自托管 docker） | 一个 OpenAI 兼容端点聚合所有模型（OpenAI 兼容端点、Claude、后续任意家）；统一密钥/限流/fallback/用量计数——多模型切换在这里配置化完成 | 直接多 SDK（代码里分叉，重放缓存键更复杂，不推荐） |
| **Agent 循环 harness** | **OpenAI Agents SDK**（Python） | 只对 LiteLLM 网关说话（OpenAI 协议），模型无关由网关承担；工具/子 Agent/handoff 原语成熟；与 Restate 有官方集成 | Pydantic AI（原生多 provider，但多一层协议差异）；自研薄循环 |
| **沙箱（Hands）** | **自托管双实现，同一 Executor 协议**：①Docker executor（dev/通用）②自托管 E2B（生产，Firecracker 微 VM，Apache-2.0） | 必须自托管：dev 用受限容器跑通逻辑；生产切 Firecracker 微 VM 拿强隔离；协议层不变、按 capability 路由 | gVisor（runsc）做容器加固中间态；Firecracker 裸用（省一层但要自写编排） |
| **API** | **FastAPI + uvicorn**（SSE 用 `sse-starlette`，WebSocket 原生） | Python 生态标准 | Litestar |
| **应用状态** | Postgres 16（含 pgvector） | 元数据 + 事件日志投影 + outbox + 长期记忆向量 | — |
| **文件/工件** | MinIO（S3 兼容，自托管） | Session 产物与快照 | 本地卷（仅 dev） |
| **事件推送** | SSE（在线）+ outbox webhook（离线） | streaming + webhook 双通道 | — |
| **控制台前端** | Next.js（唯一 TS 面，可后置） | timeline replay、会话管理、用量页 | Vite+React；或 MVP 先只做 FastAPI 自带的简单页 |
| **认证/多租户** | JWT + orgs 表 | 自托管友好 | Auth.js/Keycloak（后置） |
| **计量** | usage 事件表（活跃秒 + token + 计算秒） | 三轴计量，Stripe 后置 | — |
| **可观测** | 事件日志单一真相 + OTEL 埋点 + 时间轴 UI | 审计/重放/计量共用一份事件 | Langfuse（可选接入） |
| **部署** | docker compose：`api` `web` `restate` `litellm` `postgres` `minio` `executor` | 单机自托管；**生产沙箱加一台 Linux(KVM) 主机跑 E2B** | k8s（后置） |

## 2. 架构映射

### 2.1 Brain / Hands / Session 三向解耦

```
Brain  = run_workflow（Restate workflow，Python）+ OpenAI Agents SDK 循环
         → 所有模型调用经 LiteLLM 网关；
         每次模型调用 = 一个 journaled step，输入输出缓存进 Restate 状态
         （缓存键 = run_id + step + provider/model 版本）
         → 重放时复用输出，绝不重调模型
Hands  = Executor 协议（HTTP JSON: execute(name,input) / files / logs / snapshot）
         实现①Docker executor：受限容器（read-only root、egress 白名单、
         无 secrets、CPU/内存/时间限额、短 TTL）
         实现②E2B 自托管：Firecracker 微 VM，生产强隔离
Session = session_object（Restate Virtual Object，key=session_id）
         状态：phase、plan、记忆、模型结果缓存、恢复游标
         投影：事件 append-only 写入 Postgres events 表 → UI/审计/计量
```

铁律：**任何组件故障都不杀死会话**；凭证只在 LiteLLM/executor 注入层存在，绝不进事件日志。

### 2.2 自托管沙箱的现实约束（重要）

| 环境 | 能跑什么 | 说明 |
|---|---|---|
| macOS / 无 KVM | Docker executor（普通容器隔离） | dev 开发即可用 |
| Linux 主机 | Docker executor + gVisor（runsc）加固 | 中间态：用户态内核，隔离强于容器 |
| Linux(KVM) 服务器/云 VM | **自托管 E2B（Firecracker）** | 生产目标：<125ms 冷启动、微 VM 强隔离 |

结论：**MVP 用 Docker executor 跑通全链路（任何机器可开发），生产升级路径是同一协议下的 E2B 自托管**——这就是「任意空间」可组合性的第一次实战验收。

### 2.3 「任意时间」映射（Restate Python）

| 需求 | 原语 |
|---|---|
| cron / 一次性延迟 | `ctx.sleep()`（durable timer） |
| webhook / HITL 唤醒 | **Awakeable**（durable promise，事件 resolve） |
| 崩溃恢复 | journaled steps 自动重放续跑 |
| 空闲休眠 | 状态在 Restate/PG，进程回收，唤醒即重建 |

## 3. 数据模型（Postgres 简表，同 v1）

```sql
orgs(id, name, quotas jsonb)
api_keys(id, org_id, key_hash, scopes)
agents(id, org_id, name, config jsonb /*model/instructions/tools/mcp*/, version)
sessions(id, org_id, agent_id, status /*created|running|sleeping|paused|completed|archived*/,
         restate_key, last_active_at, ttl)
runs(id, session_id, trigger jsonb, status, started_at, finished_at)
events(id bigserial, session_id, run_id, seq, type, payload jsonb, at)  -- append-only
executors(id, org_id, kind /*docker|e2b_selfhosted*/, endpoint, capabilities jsonb, status)
schedules(id, org_id, session_id, cron, next_at, payload jsonb)
outbox(id, session_id, event_id, url, attempts, next_at)
usage(session_id, bucket /*1min*/, active_seconds, tokens_in, tokens_out, compute_seconds)
```

## 4. MVP 里程碑（4 周 × 2 人）

| 周 | 交付 | 验收 |
|---|---|---|
| W1 | compose 骨架（api/restate/litellm/pg/minio/executor）；Agent/Session 定义；纯对话 run（经 LiteLLM 走 OpenAI 兼容 + Claude 各一次） | 多模型切换在网关配置化生效；SSE 收事件 |
| W2 | Docker executor 沙箱执行 + 文件工件 + 时间轴（PG events + SSE） | Agent 写代码→跑代码→产出文件，时间轴可回看 |
| W3 | **持久性三件套**：`kill -9` 崩溃恢复、sleep/cron 唤醒、webhook HITL pause/resume | 演示：定时任务中途断电→重启→断点续跑 |
| W4 | 计量三轴 + 控制台 + demo 脚本 + （可选）Linux 主机上 E2B 自托管切流验证 | 30 分钟全闭环演示 |

## 5. 关键风险与降级

1. **Restate Python SDK 相对 TS 更新** → 若遇阻塞：`run_workflow` 迁 **Temporal + Python SDK**（其 ×OpenAI Agents SDK 集成已 GA，是研究文档验证过的组合）；Executor 协议与事件 schema 不变，迁移面隔离在持久层。
2. **多模型重放语义**：缓存键 = `run_id + step + provider + model 版本`；模型升级后运行中会话用旧缓存跑完。
3. **Docker executor 隔离有限** → 只给可信度较高的任务；E2B 自托管在 Linux(KVM) 上按 capability 路由接管不可信代码。
4. **LiteLLM 是新增单点** → 它只是无状态代理（key→provider 映射），可随 compose 重启；用量计数同时由事件日志侧兜底记录。

## 6. 与自研运行时长期路线的关系（不变）

MVP 用 Restate 验证产品形态与 Executor 协议。将来换自研持久核心（Cloudflare `runFiber` 式：状态落库 + stash 快照 + 恢复钩子 + 持久 alarm），替换边界已划好——**Executor 协议、事件日志 schema、harness 接口、Session API 全不变，只换 session_object/run_workflow 的底层实现**。

---

## 7. 附：Go 技术栈评估（对比 Python 方案）

### 7.1 一句话结论

Go 的优势**集中在平台层**（持久核心、执行空间、多租户并发、远程部署），劣势**集中在 harness 层**（agent 循环生态）。本项目两层都要，所以选 Go 与否取决于：**① 长期护城河在「运行时/平台」还是在「agent 智能」；② 团队是否有 Go 能力。**

### 7.2 Go 的四个真实优势（针对本项目）

1. **「任意空间」的落地形态**：远程 executor / agent worker 是**单个静态二进制**（~15–20MB，交叉编译到 Linux/macOS/Windows/arm）——装到防火墙后、边缘设备、客户机器上开箱即用；Python 版要靠 venv/uv/PyInstaller 折腾且体积大得多。Windmill 的 agent worker 就是「跨平台二进制」模式。**这直接强化了本项目「任意空间」的核心卖点。**
2. **多租户并发的运行经济性**：goroutine 单进程承载数万并发 session（LLM 调用是 IO-bound、SSE fan-out、沙箱调度），内存与运维成本显著低于 uvicorn 多 worker 架构。SaaS 赚的就是并发租户的钱，这是长期成本曲线优势。
3. **通往自研核心的同语言路径**：项目名是「时空可组合持久运行时」——若最终要自研 fiber 引擎 + 事件日志 + 重放（Cloudflare runFiber 式），Go 是这类基础设施的母语（Temporal 本身就是 Go）。用 Go 意味着 MVP 之后**不用换语言**就能往核心下钻。
4. **长时运行平台代码的正确性**：静态类型 + 编译期检查，对状态机、事件 schema、Executor 协议这类「跑几个月不重启」的代码是实打实的 bug 率下降。

### 7.3 Go 的两个真实劣势

1. **harness 生态薄**：没有 OpenAI Agents SDK / LangGraph / Pydantic AI 级别的 Go 框架（langchaingo 维护弱）。agent 循环、tool 管理、上下文压缩、记忆实验要自己写——这部分恰好是 Python 迭代最快的层。
2. **工具/MCP 社区生态弱**：官方 [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) 存在，但多数现成 MCP server 与工具是 Python/TS 生态；团队若无人写过 Go，`Go + LLM` 的人才池也小于 Python。

### 7.4 三种配置对比

| | A. 纯 Go | B. 混合（Go 平台 + Python harness） | C. 纯 Python（v2 当前方案） |
|---|---|---|---|
| 平台层 | Go：API/executor/调度/事件日志/网关 | Go（同左） | Python |
| 持久核心 | Restate Go SDK（活跃维护）/ Temporal Go SDK（最成熟） | 同左，harness 以多语言 endpoint 接入 | Restate Python / Temporal Python |
| harness | 手写薄循环：openai-go + anthropic-sdk-go + MCP Go SDK（~1–2k LOC） | OpenAI Agents SDK（Python） | OpenAI Agents SDK + LiteLLM |
| 「任意空间」远程 executor | ✅ 单二进制，最强 | ✅ | ⚠️ venv 打包，弱 |
| 高并发多租户经济性 | ✅ 最强 | ✅ | ⚠️ uvicorn 多 worker |
| harness 迭代速度 | ⚠️ 自写自维护 | ✅ | ✅ 最快 |
| MVP 额外成本 | +1~1.5 周（自写 harness） | 多一套服务 + 协议胶水 + 两套 CI | 0（基准） |
| 通往自研核心 | ✅ 同语言 | ⚠️ 核心仍需换 Go/Rust | ❌ 需换语言 |
| 适合 | **平台护城河路线**、有 Go 能力 | 成熟团队折中 | agent 智能路线、快速验证 |

### 7.5 Go 栈的具体拼图

```
持久核心：Restate（Go SDK，轻量优先）/ Temporal（Go SDK，最保守）
模型调用：openai-go（OpenAI 兼容端点，含 LiteLLM 网关）
        + anthropic-sdk-go（Anthropic 官方）
工具/MCP：modelcontextprotocol/go-sdk（官方）
多模型网关：LiteLLM 仍是独立 docker 服务（Python 实现但 HTTP 接口语言无关）
harness：自写薄循环（while tool_calls → 逐工具执行），
        每次模型调用 = 一个 Activity/step，结果 journal 缓存供重放复用
其余：Postgres + MinIO + Docker executor/E2B 自托管 + Next.js 控制台（不变）
部署面：3 个 Go 二进制（api / worker / executor）+ restate + postgres + minio + litellm
```

### 7.6 推荐

- **如果团队愿意上 Go 且战略重心是「运行时平台」**：选 **A（纯 Go）**。多花的 1~1.5 周 harness 成本，换来单二进制任意空间部署、并发经济性、通往自研核心的同语言路径——与本项目名的意图最一致。手写薄循环反而更贴合「harness 可替换」的主张（harness 本来就只是 while 循环 + 工具调用）。
- **如果团队主力仍是 Python、Go 只是备选**：选 **C（纯 Python）** 先出 MVP；把 Executor 协议与事件 schema 定死（本来就该如此），平台层将来有规模压力时再抽离为 Go 服务（就是 B 的演进路径）。
- **B（混合）不建议作为 MVP 起点**：两头的好处都要付出两套服务的胶水成本，适合验证成功后重构时采用。

---

## 8. 最终架构决策（v3）：Go 平台 + 借 harness（不自研）

> 决策记录：① 护城河在「运行时/平台」→ **平台层用 Go**；② 不自研 harness → **借用最成熟的开源 harness（OpenAI Agents SDK，Python），独立成无状态服务**。即 §7.4 中的 B，但以「harness 无状态化 + 单协议对接」把胶水成本压到最低。

### 8.1 最终组件表

| 层 | 选型 |
|---|---|
| 平台（API/会话注册/调度/计量/事件日志/outbox） | **Go**（Hono 等价物：net/http + chi；SSE 手写） |
| 持久核心 | **Restate（Go SDK，活跃维护）**；降级路径 **Temporal 多语言**（Go workflow 调 Python task queue 是官方成熟模式） |
| 执行空间（executor） | Go 二进制：Docker executor（dev）→ 自托管 E2B（生产）；**单二进制 = 任意空间部署** |
| harness 服务 | Python FastAPI + **OpenAI Agents SDK**（借用，不自研），经 LiteLLM 网关调模型 |
| 对接协议 | 仅一条：`POST /runs {session_id, messages, tools, run_step}` → SSE 事件流；Go 侧带幂等键调用 |
| 工具路由 | API 类工具在 harness 内执行；**代码/命令类工具一律走 Go executor**（Brain/Hands 协议，harness 内置 code 工具禁用） |
| 其余 | Postgres(+pgvector)、MinIO、LiteLLM（docker）、Next.js 控制台（唯一 TS 面） |

### 8.2 三条关键纪律（借 harness 而不被 harness 绑死）

1. **harness 无状态化**：Session 状态 100% 归 Go 平台持久层（Restate 对象 + Postgres 事件表）。harness 每次调用 =「输入(会话消息+工具清单) → 输出(回复+tool_calls)」，进程被杀、重启即失忆也无所谓。OpenAI Agents SDK 自带的 Session 特性**不使用**——这是把 Brain/Hands/Session 三解耦落到进程边界上。
2. **幂等 + 输出缓存 = 重放语义**：Go 侧以 `run_id + step + provider/model版本` 为键，把 harness 输出 journal 进 Restate 状态。崩溃重放时直接回放缓存，**不重调 harness 也不重调模型**——持久执行的核心保证不变，且与 harness 的实现细节完全解耦。
3. **harness 可替换成为运行时保证**：平台只认 `POST /runs` 一条协议。将来换 LangGraph / Pydantic AI / 甚至托管 harness，平台层零改动。

### 8.3 部署面与 MVP 成本

- 部署面：**3 个 Go 二进制**（api / worker / executor）+ **1 个 Python 服务**（harness）+ restate + postgres + minio + litellm，compose 一条命令。
- 成本：比纯 Python v2 方案多约 **+0.5~1 周**（harness 服务骨架 + /runs 协议），远低于纯 Go 自写 harness 的 +1~1.5 周；且 harness 本身是现成 SDK 的标准代码。
- 里程碑不变：W1 额外交付「harness 服务 + /runs 协议 + 多模型切换验证」。

### 8.4 风险与降级

1. **双语言 CI/打包**：Go 交叉编译产物 + Python 一个 Docker 镜像，无额外工具链（不引入 gRPC，HTTP+SSE 即可）。
2. **Restate Go SDK 相对年轻** → 降级 Temporal 多语言（成熟度最高、官方支持 Go workflow + Python activity）。
3. **harness 服务故障**：无状态 → Go 侧按 Restate retry policy 重试即可，恢复后从缓存断点继续，绝无状态分裂。
