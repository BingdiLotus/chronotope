# 调研报告：可在任意时间、任意空间持久执行的 Agent SaaS 服务应该长什么样

> 调研日期：2025-10 · 项目正式名 **Chronotope**（中文副标题：时空可组合持久运行时）的前期调研
> 核心问题：一个「任意时间（any time）· 任意空间（any space）· 持久执行（durable execution）」的 Agent SaaS 服务，其概念边界、竞品格局、架构模式与产品形态应该是什么？

---

## 0. TL;DR（核心结论）

1. **「任意时间 + 任意空间 + 持久执行」在今天的市场上没有单一完整产品，但每一块都有成熟参照物。** 持久执行看 Temporal / Inngest / Cloudflare Durable Objects；托管 Agent 平台看 OpenAI Agents API / Claude Managed Agents / AWS Bedrock AgentCore；任意空间看 Windmill Agent Workers / OpenAI 自托管 Sandbox / A2A 协议；任意时间看 Temporal Schedules / Inngest cron / Cloudflare Alarms / ChatGPT Scheduled Tasks。真正的产品机会在于把这些拼图组合成一个可组合的整体。
2. **业界正在收敛到一个核心架构：Brain / Hands / Session 三向解耦。** Brain（模型 + harness）无状态、可替换；Hands（沙箱/执行环境）统一抽象为 `execute(name, input) → string`、可插拔到任何空间；Session（会话日志）只追加（append-only）、放在上下文窗口之外，是持久性的根基。设计原则：**任何一个组件故障都不能杀死会话**。
3. **持久执行有两派工程路线，且都被验证可行：**
   - **工作流派**（Temporal / Inngest）：事件溯源 + 确定性重放，Workflow 编排 + Activity 副作用，崩溃后从事件历史恢复；每个 Agent 调用包装为一个 Activity（Temporal × OpenAI Agents SDK 已 GA）。
   - **Actor/对象派**（Cloudflare Durable Objects / Agents）：单写者 + SQLite 持久状态 + 显式 checkpoint（`stash`）+ 恢复钩子 + 闹钟心跳保活/唤醒。两派可以组合。
4. **「任意空间」的本质是执行环境的可插拔协议**，而不是自建所有机房：托管微 VM 沙箱（Firecracker 系）、客户自有 VPC/BYOC、防火墙后的远程 worker（HTTP + JWT）、边缘节点、用户设备，应该共用同一套 Executor 接口与事件协议。
5. **SaaS 化的关键设计点**：Session 是一等公民（可暂停、唤醒、继续、steer）；计费以「活跃 session 时长」为主轴（Claude 定价 8¢/session-hour）辅以 token 与计算秒；空闲即休眠、唤醒在秒级（scale-to-zero）；多租户靠「每 session 一个微 VM」隔离；审计靠只追加事件日志（事件日志天然就是审计轨迹）。
6. **最大的护城河与最大的风险是同一件事：harness。** 研究显示 harness 工程带来的性能差异可达 6 倍，且优化后的 harness 可跨模型迁移。买 Tier 3 托管平台 = 同时交出 memory / infra / harness 三面，迁移成本极高；自建则要正面承担 harness 工程与持久执行引擎的成本。

---

## 1. 概念界定：把「任意时间 · 任意空间 · 持久执行」拆成可工程化的需求

这句话实际上包含四个正交的维度，每一个都能映射到具体的技术机制：

### 1.1 时间维（任意时间）——何时执行、执行多久、中断后怎么办

| 需求 | 技术机制 | 业界参照 |
|---|---|---|
| 按计划执行（cron / 一次性延迟） | 调度器 + 持久定时器 | Temporal Schedules、Inngest cron triggers、Cloudflare `setAlarm`、ChatGPT [Scheduled Tasks](https://help.openai.com/en/articles/10291617-scheduled-tasks-in-chatgpt) |
| 由事件唤醒（webhook / 消息 / MCP 事件 / A2A） | 事件总线 + `waitForEvent` 原语 | Inngest 的 [step.waitForEvent](https://www.inngest.com/docs/durable-execution)、OpenAI Agents API 的 [Webhooks](https://developers.openai.com/api/docs/guides/agents-api/sessions/webhooks) |
| 长时间运行（分钟级单任务、小时级会话、天级工作流） | 任务与进程生命周期解耦 | Temporal workflow 可跑数天；OpenAI session 跨轮次持续 |
| 崩溃/重启/发版/驱逐后继续执行 | 持久执行（checkpoint + 恢复） | Temporal 事件历史重放、Cloudflare runFiber、LangGraph checkpoint |
| 空闲即休眠、被唤醒时秒级恢复 | 状态落盘 + 冷启动优化 | Durable Objects hibernation（SQLite 保留）、Anthropic lazy container provisioning |
| 执行中插入控制（暂停/审批/改道） | Update/Signal/Interrupt 原语 | Temporal Update/Query、LangGraph interrupt/HITL、OpenAI「steer the agent」 |

### 1.2 空间维（任意空间）——在哪里执行

「任意空间」不是「我自己建所有机房」，而是**执行环境的可插拔性**。业界已收敛出四种空间形态：

| 空间 | 形态 | 参照 |
|---|---|---|
| 托管沙箱（云端，不可信代码） | Firecracker/gVisor 微 VM，API 秒级创建 | [E2B](https://e2b.dev/)（Firecracker，<125ms boot）、Modal Sandboxes、[Vercel Sandbox](https://vercel.com/docs/sandbox) |
| 客户自有环境（VPC / BYOC / 本机 / Lambda） | 自托管 executor，反向连接控制面 | OpenAI Agents API `environment.type: "self_hosted"`（[架构文档](https://developers.openai.com/api/docs/guides/agents-api/architecture.md)）、Qovery/Northflank BYOC |
| 防火墙后 / 弱网远程节点 | 只出站 HTTP + JWT 的 agent worker | [Windmill Agent Workers](https://www.windmill.dev/docs/core_concepts/agent_workers.md)（`MODE=agent`，无需数据库直连，可跑在不可信站点） |
| 另一个 Agent / 远程服务 | 智能体间协议 | [A2A 协议](https://learn.microsoft.com/en-us/agent-framework/journey/agent-to-agent)（Google Vertex AI Agent Engine 原生支持，[公告](https://discuss.google.dev/t/launched-the-a2a-protocol-is-now-natively-integrated-on-vertex-ai-agent-engine/264045)）、MCP |

关键洞察：**「数据不出客户 VPC」是受监管行业的 day-one 架构约束，不是后补项。** 业界共识（AI Engineer 大会，2026-08）是：编排层只存事件与引用（reference + schema），载荷放在客户环境里——「the data never comes to us」必须是第一天就成立的架构假设（[agent-platform-tiers](https://raw.githubusercontent.com/tim-kaa-py/ai-wiki/refs/heads/main/wiki/concepts/agent-platform-tiers.md)）。

### 1.3 持久维（持久执行）——状态如何穿越故障

| 机制 | 说明 | 参照 |
|---|---|---|
| 事件溯源 + 重放 | 所有副作用记录进 append-only 事件日志；崩溃后重放到断点 | Temporal（[what is durable execution](https://temporal.io/blog/what-is-durable-execution)） |
| 显式 checkpoint | 开发者显式保存恢复快照，恢复钩子决定续跑策略 | Cloudflare `runFiber` + `ctx.stash()`（[forever.md](https://github.com/cloudflare/agents/blob/b504eed1/experimental/forever.md)） |
| 会话日志（session log） | 会话历史只追加、置于模型上下文之外，模型可回查而不必压缩 | Anthropic [Managed Agents](https://www.anthropic.com/engineering/managed-agents) 设计 |
| 记忆分层 | 短期（会话内）+ 长期（跨会话持久 store + 文件系统记忆） | AWS Bedrock AgentCore [Memory](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory.html)、LangGraph checkpointers + stores |

### 1.4 可组合维——哪些部分能换

参照五层「自建—购买」谱系（[Agent Platform Tiers](https://raw.githubusercontent.com/tim-kaa-py/ai-wiki/refs/heads/main/wiki/concepts/agent-platform-tiers.md)），锁定的三个表面是 **Memory / Infra / Harness**。一个可组合的服务应当让这三者各自可替换：模型可换、harness 可换、沙箱可换、记忆后端可换——这是与「模型锁定的 Claude」「SaaS 锁定的 LangSmith」形成差异化的核心。

---

## 2. 竞品格局：三个赛道 × 四类玩家

### 2.1 托管 Agent 平台（Tier 3：harness + infra + memory 全托管）

| 平台 | 架构特征 | 计费 | 锁定面 |
|---|---|---|---|
| **Claude Managed Agents**（Anthropic） | Brain/Hands/Session 三向解耦；`wake(sessionId)` + `getSession(id)` 恢复；凭证保管库独立于沙箱；懒加载容器把 p50 TTFT 降 60%、p95 降 90%+ | **8¢ / session-hour**（活跃时长）+ 模型费 | 模型锁死 Claude（[对比](https://raw.githubusercontent.com/tim-kaa-py/ai-wiki/69e074727307d2b8d19bcf9b58dec9a74cc40e30/wiki/tools/claude-managed-agents.md)） |
| **OpenAI Agents API** | 托管 Codex harness；四概念：Agent / Environment / Session / Events；environment 三选一：`none` / `openai_hosted` / `self_hosted`；streaming + webhooks；上下文压缩、子 Agent 委派、会话续跑 | 模型费率 + 内置工具费率 + 容器费率 | 前端模型偏置；数据仅美国区（[docs](https://developers.openai.com/api/docs/guides/agents-api/overview.md)） |
| **AWS Bedrock AgentCore** | 模块化全家桶：Harness、Runtime（微 VM/会话隔离）、Memory、Gateway（API→MCP）、Identity、Browser、Observability、**Payments（x402 机器支付）**、Evaluations、Policy | 用量计费、无最低消费 | 云锁定（但模型无关，[overview](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/what-is-bedrock-agentcore.html)） |
| **LangGraph Platform / LangSmith** | 有状态图 + Postgres checkpoint 持久执行；cron、double-texting、HITL、scale-to-zero；GA 主打「长时运行的有状态 Agent」（[公告](https://blog.langchain.dev/langgraph-platform-ga/)） | $39/座/月（LangSmith Plus）+ 用量 | LangSmith SaaS 锁定；Deep Agents 库本身 MIT |
| **Google Vertex AI Agent Engine** | 托管开源框架集成（LangGraph/ADK 等）+ 原生 A2A | 用量计费 | 云锁定，模型无关 |

**共同点**：全部围绕「Session 为一等公民 + 可插拔沙箱 + 事件驱动的进度反馈」设计；差异只在锁定面和成熟度。OpenAI 的 [architecture](https://developers.openai.com/api/docs/guides/agents-api/architecture.md) 文档是理解这一代产品的最清晰蓝本：harness 托管在厂商侧，environment 从 `none`（纯工具）到 `openai_hosted`（托管沙箱）到 `self_hosted`（客户 executor 反向接入），应用侧只负责发任务、收事件、处理 function tool。

### 2.2 持久执行引擎（Tier 2：基础设施，自己拼 harness）

| 引擎 | 模型 | 与 Agent 的结合点 | 备注 |
|---|---|---|---|
| **Temporal** | Workflow（确定性编排，重放）+ Activity（副作用）；事件历史即审计 | **OpenAI Agents SDK 集成已 GA**：每次 Agent 调用自动包装为一个 Activity，崩溃后免费续跑、可中途修 bug 继续执行；同时获得横向扩容（每个微 Agent 独立进程） | 刚以 $12.55B 估值融资 $550M，AI 是主驱动力（[博客](https://temporal.io/blog/announcing-openai-agents-sdk-integration)） |
| **Inngest** | Durable workflows / agents / endpoints；`step.run`、`sleep`、`sleepUntil`、`waitForEvent`、cron；自动 checkpoint | AI inference step 原生支持 | 类型最全的触发/等待原语（[docs](https://www.inngest.com/docs/durable-execution)） |
| **Cloudflare Durable Objects + Agents** | 单写者 actor + SQLite + alarm；`keepAlive()` 心跳防驱逐；`runFiber()` 注册到 SQLite、`stash()` 全量替换快照、`onFiberRecovered()` 恢复钩子；hibernation 后 SQLite 与 alarm 均存活 | 专门针对 LLM 流中断的恢复策略：OpenAI 用 Responses `store:true` 存 responseId 恢复；Anthropic 用合成续写消息 | 「forever」设计文档是 actor 派持久执行的教科书（[forever.md](https://github.com/cloudflare/agents/blob/b504eed1/experimental/forever.md)） |
| **Windmill** | 代码优先编排 + worker 池 + **Agent Workers**（远程执行节点） | 调度、队列、scale-to-zero、自定义运行时 | 开源自托管，Enterprise 才解锁 agent worker |

**两派路线对比（本项目选型关键）**：

| | 工作流派（Temporal/Inngest） | Actor 派（Durable Objects） |
|---|---|---|
| 状态模型 | 事件历史，确定性重放 | 显式快照 + 恢复钩子 |
| 开发者心智 | 代码要确定性，副作用放 Activity | 单写者对象，stash 你想恢复的一切 |
| 并发/多实例 | 天然分布式，多 worker 池 | 单 actor 串行，需分片 key |
| LLM 非确定性 | 结果记入事件历史，重放复用（不重调模型） | 快照存 responseId 等游标 |
| 绑定 | 独立部署或 Temporal Cloud | 绑定 Cloudflare 平台 |

结论：**LLM 调用天然非确定性，持久执行引擎必须把「模型输出」当作已记录的事实来重放，而不是重新调用模型。** 这是 Agent 场景对传统 workflow 引擎最大的改造点。

### 2.3 沙箱/执行环境（Hands 层）

九个平台的对比（[Qovery 对比文](https://www.qovery.com/blog/coding-agent-ephemeral-dev-environments-9-platforms-compared)，2026-09 数据）的核心结论：

- **冷启动**：E2B（Firecracker，[<125ms boot](https://firecracker-microvm.github.io/)）、Daytona（宣称 <90ms 创建）、Vercel Sandbox（Firecracker）——微 VM 池化是「任意空间秒级到位」的技术底座。
- **TTL/成本**：默认 5 分钟空闲回收、Pro 上限 24h；**成本由「环境小时数」驱动**，因此 TTL、merge 拆除、空闲自停是决定账单的三个功能。
- **隔离强度排序**：Firecracker 微 VM > gVisor > 专用容器 + 出站默认拒绝 > 共享 CI runner shell（不要做）。45% 的 AI 生成代码引入 OWASP Top 10 漏洞（[Veracode](https://www.veracode.com/blog/genai-code-security-report/)），所以「不可信代码沙箱」是 Hands 层的默认假设。
- **开源是真实选型标准**：E2B 是 Apache-2.0 可自托管；Daytona 2026-06 起核心转闭源——「能自托管」要读实际分支的 license 而非 badge。
- **两类环境是互补的**：内环（写-跑-测，微 VM 沙箱，秒级，无共享 URL）vs 外环（每 PR 一个预览环境，带数据库与 HTTPS，供人评审）。Agent SaaS 至少要覆盖内环，外环可选。

### 2.4 「任意时间」的调度/唤醒产品化参照

- **ChatGPT Scheduled Tasks**（[help 文档](https://help.openai.com/en/articles/10291617-scheduled-tasks-in-chatgpt)）：面向 C 端验证了「定时/周期任务」是用户可感知的 Agent 能力。
- **Cloudflare Alarms**：`setAlarm` 持久化到磁盘，驱逐后仍触发——「时间」本身被持久化。
- **Temporal Schedules / Inngest cron**：B 端标准的调度原语。
- 缺口：**「事件驱动的任意唤醒 + 持久会话 + 任意空间执行」三者组合**，目前没有产品完整打通（MCP Events 作为唤醒源的提案仍处于讨论阶段，见 [hermes-agent#130124](https://github.com/NousResearch/hermes-agent/issues/130124)）。

---

## 3. 目标产品应该长什么样：需求清单

### 3.1 资源模型（四层）

```
Org / Workspace（多租户、RBAC、配额）
 └─ Agent（模型 + 指令 + 工具/MCP + 技能 + 凭证引用；不可变版本化）
     └─ Session（持久实例：会话日志 + 环境引用 + 调度/触发绑定）
         └─ Run / Turn（一次任务执行：事件历史、checkpoint、可恢复）
```

借鉴 OpenAI 的 Agent/Environment/Session/Events 四概念与 Anthropic 的 Session 恢复原语（`wake(sessionId)` / `getSession(id)`），Session 必须具备：**暂停、休眠、唤醒、续跑、steer（中途改道）、分叉**。

### 3.2 时间轴能力（任意时间）

1. **触发**：HTTP / webhook / cron / 一次性延迟 / 事件总线订阅 / MCP 事件 / A2A 请求，统一为「事件 → 唤醒 session」。
2. **执行中**：分钟级到天级任务；崩溃、发版、驱逐、断网后自动续跑（事件重放或快照恢复）。
3. **空闲**：自动休眠（状态落盘，进程回收），唤醒秒级恢复（微 VM 快照/池化 + 懒加载）。
4. **时间原语**：`sleep` / `sleepUntil` / `waitForEvent` / `waitForSignal` 作为 SDK 一等公民。
5. **审计**：时间轴可视化（Timeline replay），任何时刻可回看「谁在什么时候做了什么」。

### 3.3 空间轴能力（任意空间）

1. **Executor 抽象**：统一 `execute(name, input) → string` 接口（Anthropic Hands 设计），后端可插拔：
   - 托管微 VM 池（Firecracker，多区域）
   - 客户自托管 executor（反向长连接 + JWT，参考 Windmill Agent Worker）
   - BYOC（客户 VPC/K8s namespace）
   - 边缘节点 / 设备端（A2A 或轻量 executor）
2. **连接降级**：弱网/防火墙后可用（HTTP-only、重试、断线重连续传），Windmill 已验证可支撑千级远程 worker。
3. **数据驻留**：编排层只存引用，载荷可配置留在指定区域/客户环境（day-one 架构假设）。
4. **能力分层**：同一 session 可声明「需要 GPU」「需要私有网络」「需要浏览器」，调度器按能力路由到空间。

### 3.4 持久轴能力

1. **事件日志**：append-only，是审计、重放、计费的单一事实来源。
2. **Checkpoint**：自动（每步）与显式（`stash`）双模式；快照含恢复游标（responseId、sandbox sessionId、消息 offset）。
3. **LLM 流恢复**：中断的流式生成按厂商策略恢复（OpenAI 存响应 ID 取回；Anthropic 持久化部分输出 + 续写消息）——参照 Cloudflare 的按厂商恢复矩阵。
4. **记忆**：会话内（短期）+ 跨会话长期 store + 文件系统记忆，带版本历史与归属（谁写了哪条）。
5. **升级策略**：代码/配置版本化，运行中的会话绑定到旧版本跑完或显式迁移（Temporal versioning 同款问题）。

### 3.5 SaaS 产品形态

| 维度 | 设计 |
|---|---|
| 计费 | 主轴：**活跃 session 时长**（参照 Claude 8¢/h）+ token（模型费）+ 计算秒（沙箱费）；空闲休眠不计费；预付费额度 + 硬上限熔断 |
| 多租户隔离 | 每 session 一个微 VM；per-org 资源配额、速率限制、网络策略 |
| 安全 | 凭证保管库独立于沙箱（生成代码触不到 secrets）；出站网络白名单；prompt injection 护栏；审批流（HITL） |
| 可观测 | 事件流 + OTel/OpenInference 追踪 + eval（每 agent 质量信号） |
| 协议 | 出：MCP（工具）、A2A（智能体间）、Webhooks（事件）；入：同一套 |
| 开发者体验 | SDK（Python/TS）为主、REST/streaming 兜底；本地开发与生产行为一致（Cloudflare 的 E2E：SIGKILL 后重启验证恢复） |

---

## 4. 参考架构（时空可组合持久运行时）

```
                        ┌────────────────────────────────────────────┐
   SDK / REST / A2A ──▶ │  控制面 Control Plane                        │
   MCP / Webhook   ──▶ │  API GW · 身份/RBAC · Session 注册表          │
                        │  调度器(cron/delay/alarm) · 事件总线          │
                        │  计费/配额 · 审计                                │
                        └──────────────┬─────────────────────────────┘
                                       │ 只追加事件日志（WAL）
                        ┌──────────────▼─────────────────────────────┐
                        │  持久层 Durability                            │
                        │  事件历史 · Checkpoint store · 重放引擎        │
                        │  (Postgres / SQLite / 对象存储)                │
                        └──────────────┬─────────────────────────────┘
                                       │ 按能力路由
        ┌──────────────┬──────────────┼──────────────┬──────────────┐
        ▼              ▼              ▼              ▼              ▼
  托管微VM池     客户自托管Executor   BYOC      边缘/设备端      远程Agent
  (Firecracker)  (反连+JWT worker)  (VPC)     (轻量运行时)     (A2A)
        │              │              │              │              │
        └──────────────┴──────┬───────┴──────────────┴──────────────┘
                              ▼
                   统一 Executor 协议: execute(name, input) → string
                   统一 Event 协议:   events out (stream/webhook)
                              │
                    ┌─────────▼──────────┐
                    │ Harness（模型无关）  │  记忆层：会话日志 + 长期store
                    │  agent loop · 子agent │  + 文件系统（版本/归属）
                    │  上下文压缩 · 护栏     │  凭证保管库（独立于沙箱）
                    └────────────────────┘
```

**分层原则**：

1. **持久层是地基**：一切状态（会话、进度、计划、待办、记忆）先落事件日志，再谈执行。执行层任何组件可死，事件日志不可死。
2. **执行层是可插拔的空间**：同一接口背后的实现从托管微 VM 到客户设备自由切换；「任意空间」= 路由 + 协议，而非自建一切。
3. **Harness 与空间解耦**：模型循环不绑定执行环境（OpenAI 的 `environment.type: none` 已证明 harness 可脱离沙箱独立工作）。
4. **事件流是唯一对外真相**：streaming 用于实时 UI，webhook 用于无人值守，两者消费同一事件日志的投影。

**关键技术选型权衡**：

| 决策点 | 选项 A | 选项 B | 建议 |
|---|---|---|---|
| 持久化模型 | 确定性重放（Temporal） | 显式快照（DO fiber） | 混合：编排用重放，Agent 会话用快照+恢复钩子（LLM 不可重放） |
| 状态存储 | 单库 Postgres | 每 session 一个 SQLite（DO 式） | 后者单写者简单、隔离好、可随 session 迁移（「任意空间」友好） |
| 沙箱 | 自建 Firecracker 池 | 采购 E2B/Modal + 自托管协议 | 先用采购保速度，Executor 协议保留自建位 |
| 唤醒 | 定时轮询 | 持久 alarm + 事件推送 | alarm 持久化到日志，重启后可恢复（Cloudflare 已验证） |

---

## 5. 差异化机会与开放问题

### 5.1 市场空白（机会）

- **「任意空间」无人完整覆盖**：Temporal 是 SDK 不是空间；DO 绑定 Cloudflare；托管平台的空间选项只有「托管沙箱 / 单一自托管」两档。真正支持「边缘 + 客户 VPC + 设备 + 托管沙箱」统一协议的产品不存在。
- **持久执行与 Agent 平台的缝合**：Temporal×OpenAI SDK 证明需求真实，但它是「你自建」方案；提供开箱即用的「持久 Agent SaaS」仍是蓝海。
- **计费透明度**：8¢/session-hour 式定价尚未成为标准，透明计费（活跃时长 + token + 计算秒三项分列）是信任优势。
- **可组合中立性**：模型中立 + harness 可换 + 沙箱可换 + 记忆可换，是对抗三巨头各自锁定的定位空隙。

### 5.2 最难的问题（诚实清单）

1. **LLM 非确定性与重放的张力**：重放必须复用已记录的模型输出；这要求把「模型调用」建模为带缓存的副作用，且缓存键要覆盖模型版本与参数——版本升级后旧会话的行为漂移如何处理？
2. **运行中代码发版**：持久会话跨数天时，SDK 代码必然发版；会话绑定版本跑完（Temporal versioning）vs 热迁移，需要明确语义。
3. **任意代码执行作为服务**的滥用风险：挖矿、垃圾邮件、网络扫描——需要资源配额、出站审计、人工审批与 kill-switch。
4. **多租户冷启动池**：为保持「唤醒秒级」需要温池微 VM，池的预热策略与成本权衡是运营难题（Anthropic 用懒加载把 p50/p95 降 60%/90%，值得学）。
5. **跨空间状态迁移**：session 从云端迁到客户 VPC 或反之，事件日志与文件状态的同步/一致性协议。
6. **harness 归属**：harness 工程是 6x 性能差与可迁移 IP；作为 SaaS 提供「默认 harness + 可替换」比「锁定 harness」更难做但更持久。

---

## 6. 结论：一句话产品定义

> **一个以「只追加会话日志 + 事件溯源」为地基，以「可插拔 Executor 协议」为空间边界，以「活跃时长计费的 Session 生命周期（创建→任务→休眠→唤醒→续跑→归档）」为主轴，harness、模型、沙箱、记忆四者可替换的模型中立 Agent SaaS。**

对照本项目名「时空可组合持久运行时」：**时** = 调度/唤醒/恢复的时间轴；**空** = Executor 可插拔的空间轴；**持久** = 事件日志 + checkpoint 的地基；**可组合** = Brain/Hands/Session 三向解耦与四者可替换；**运行时** = 托管 harness + 微 VM 执行平面。

---

## 7. 主要参考资料

**托管 Agent 平台**
- OpenAI Agents API：[Overview](https://developers.openai.com/api/docs/guides/agents-api/overview.md) · [Architecture](https://developers.openai.com/api/docs/guides/agents-api/architecture.md) · [Agents guide](https://developers.openai.com/api/docs/guides/agents)
- Anthropic：[Scaling Managed Agents](https://www.anthropic.com/engineering/managed-agents)（Brain/Hands/Session 三向解耦）；[Claude Managed Agents 综述](https://raw.githubusercontent.com/tim-kaa-py/ai-wiki/69e074727307d2b8d19bcf9b58dec9a74cc40e30/wiki/tools/claude-managed-agents.md)
- [Managed Agent Platforms: Claude vs LangChain vs OpenAI](https://raw.githubusercontent.com/tim-kaa-py/ai-wiki/refs/heads/main/wiki/comparisons/managed-agent-platforms.md)（三方对比矩阵）
- AWS：[Bedrock AgentCore Overview](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/what-is-bedrock-agentcore.html)
- Google：[Vertex AI Agent Engine × A2A 公告](https://discuss.google.dev/t/launched-the-a2a-protocol-is-now-natively-integrated-on-vertex-ai-agent-engine/264045)
- LangChain：[LangGraph Platform GA](https://blog.langchain.dev/langgraph-platform-ga/) · [Persistence docs](https://docs.langchain.com/oss/python/langgraph/persistence)

**持久执行引擎**
- Temporal：[What is durable execution](https://temporal.io/blog/what-is-durable-execution) · [OpenAI Agents SDK 集成（已 GA）](https://temporal.io/blog/announcing-openai-agents-sdk-integration) · [Temporal $12.55B 融资](https://temporal.io/blog/temporal-raises-usd550m-series-e-at-usd12-55b-valuation-ai)
- Inngest：[Durable Execution docs](https://www.inngest.com/docs/durable-execution) · [Durable endpoints](https://www.inngest.com/durable-endpoints)
- Cloudflare：[Agents — forever.md（长时运行 Agent 设计）](https://github.com/cloudflare/agents/blob/b504eed1/experimental/forever.md) · [Long-running agents docs](https://developers.cloudflare.com/agents/concepts/agentic-patterns/long-running-agents/)
- Windmill：[Agent Workers](https://www.windmill.dev/docs/core_concepts/agent_workers.md)（防火墙后远程执行）

**沙箱与执行环境**
- [Qovery：9 个 Agent 临时环境平台对比](https://www.qovery.com/blog/coding-agent-ephemeral-dev-environments-9-platforms-compared)（冷启动/隔离/TTL/成本矩阵）
- [E2B](https://e2b.dev/) · [Firecracker](https://firecracker-microvm.github.io/) · [Veracode GenAI 代码安全报告](https://www.veracode.com/blog/genai-code-security-report/)

**概念框架**
- [Agent Platform Tiers（五层自建—购买谱系）](https://raw.githubusercontent.com/tim-kaa-py/ai-wiki/refs/heads/main/wiki/concepts/agent-platform-tiers.md)
- [A2A：Agent 间通信（Microsoft Agent Framework）](https://learn.microsoft.com/en-us/agent-framework/journey/agent-to-agent)
- [ChatGPT Scheduled Tasks](https://help.openai.com/en/articles/10291617-scheduled-tasks-in-chatgpt)
