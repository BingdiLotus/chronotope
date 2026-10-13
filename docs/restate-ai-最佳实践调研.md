# Restate AI 官方最佳实践调研（值得学习的清单）

> 2026-10-13 深度调研 [docs.restate.dev/ai](https://docs.restate.dev/ai) 的
> 官方教程与示例（HITL / Error Handling 两页全文 + 目录全览）——对照
> Chronotope 现状梳理值得学习的实践。

## 一、官方模式全景（目录）

```
Getting Started：Durable Agents / Durable Sessions / HITL（Pause & Resume）/
  Multi-Agent / Observability & Control
Orchestration：Parallel Tool Calls / Workflows / Workflows as Tools /
  Remote Agents / Racing agents / Interrupt & Regenerate
Reliability：Retries & Error Handling / Rollback on Failures
Frontend：Chat UI / Streaming / Notify when ready
SDK 集成：Vercel AI / OpenAI Agents / Google ADK / Pydantic AI / LangChain /
  LiteLLM（六个官方集成）
可观测生态：Langfuse / Logfire / Phoenix / Parseable
```

## 二、逐项对照（已对齐 vs 值得学习）

| 官方实践 | Chronotope 现状 | 判定 |
|---|---|---|
| Awakeable 审批（HITL） | awaitApproval 的 awakeable + DECIDE 先行 | ✅ 已对齐（且更强——DECIDE 先行） |
| **审批超时（orTimeout / select race）** | awaitApproval **无超时**（挂起无限） | ⭐ **值得学习 ①**——超时 + 降级路径（「Approval timed out - Evaluate with AI」的 SLA 语义） |
| **LLM 调用显式重试上限（maxRetryAttempts: 3）** | SDK 默认重试的 hazard（九期 4.1 已 terminal 化部分） | ⭐ **值得学习 ②**——transient 有界重试（官方明示「LLM calls are costly——避免无法自愈的重试」） |
| Transient/Terminal 两态 | ToTerminalError 的 terminal 化 + unknown 显式停 | ✅ 已对齐（官方语义与我们的 terminal 纪律一致） |
| 工具 transient 重试（ctx.run 包装） | executor 的 in-flight 重试（30×10s） | ✅ 已对齐 |
| stopWhen（步骤上限 + terminal 停止） | maxSteps + 工具的 terminal 停 | ✅ 已对齐 |
| **工具错误 → LLM 消息的默认语义** | exec 错误 → failRun（terminal 直接失败） | ⭐ **值得学习 ③**——官方默认「工具错误转消息给 LLM 决策（fallback/换工具）」——我们的 dispatchSubagent 有此语义但 exec 没有 |
| Rollback/Compensation | 可补偿层（评审二三态谱系——后置） | 🔧 后置项（官方 rollback 页的具体形态可参考） |
| parallelToolCalls: false（审批场景） | runLoop 顺序 dispatch | ✅ 已对齐 |
| SDK 集成（六官方集成） | 四 harness 适配器（Claude/Codex/Pi/dsh） | ✅ 已对齐（我们的适配器层等价） |
| 可观测生态（Langfuse 等四家） | OTel 三 span（stdout/OTLP） | 🔧 可学习（生态集成的 exporter 形态——B4 的 OTLP 后置项触发） |

## 三、值得学习的三项（按优先级）

### ① 审批超时 + 降级路径（SLA 语义）

- **官方形态**：`approval.promise.orTimeout({hours: 3})` 或
  `restate.select(approval, timeout)`——超时后「Evaluate with AI」的降级
- **我们的缺口**：awaitApproval 挂起无限——审批人消失的 run 永远挂起
- **落地形态**：awaitApproval 加超时参数（org 策略缝的 TTL——审批的
  SLA）+ 超时降级（拒绝 + 事件 + 模型可决策——或 AI 继续的显式策略）

### ② LLM 调用显式重试上限（成本纪律）

- **官方形态**：`RunOptions(max_attempts=3)`——transient 有界重试 +
  terminal 立即失败 + handler 优雅降级（「couldn't complete: msg」）
- **我们的缺口**：llm_calls 的 transient 错误进 SDK 的无限/默认重试
  （九期 4.1 只 terminal 化了部分路径）
- **落地形态**：harnessCallClosure 的 transient 错误（网络/限流）的
  有界重试（3 次指数退避）+ 重试耗尽 → unknown 显式停 + run 的优雅
  降级事件

### ③ 工具错误 → 模型决策的默认语义

- **官方形态**：工具执行错误默认转消息给 LLM——「agent decides how to
  proceed（不同工具或 fallback 答案）」
- **我们的缺口**：exec 的错误直接 failRun（terminal）——模型的决策权
  被剥夺（dispatchSubagent 的「错误回喂父 agent 可决策」是例外）
- **落地形态**：exec 的非 terminal 错误（transient 耗尽后的可恢复
  错误）→ tool result 消息回喂模型（下一 step 决策）——terminal 错误
  （业务违规/权限）保持 failRun

## 四、诚实判定

- **Chronotope 的 durable 基础已与官方同源**（restate-sdk-go 的
  awakeable/run/journal——HITL 与重试的基础设施相同）
- **官方值得学习的不是「durable 机制」而是「AI 层的成本与 SLA 纪律」**
  ——超时降级、重试上限、错误给模型决策——三项都是「模型调用是昂贵且
  不确定的」这一事实的工程化
- 三项的学习成本低（每项是策略缝的扩展——超时/重试上限/错误路由的
  参数化）——按序落地

## 五、分阶段落地（按序）

```
① 审批超时（awaitApproval 的 TTL + 降级路径）
② LLM 重试上限（transient 有界 + unknown 显式停）
③ 工具错误的模型决策路由（exec 的非 terminal → 回喂）
④（触发式）Rollback 的具体形态 + 可观测生态的 exporter 集成
```


## 附：restatedev/agent 参考架构调研（2026-10-13 追加）

> [github.com/restatedev/agent](https://github.com/restatedev/agent)——
> 「A durable AI agent on Restate」的 TypeScript 参考实现（Agent +
> AgentSession 两个 Virtual Object——与我们的 session_object +
> run_workflow 等价）。

### 六特性对照（官方核心主张）

| 官方主张 | Chronotope | 判定 |
|---|---|---|
| STEERABLE（handle 稳定 + interrupt 传播 subagents） | steering 队列 + cancel | ✅ 部分（interrupt 的「总结已完成」语义待补） |
| STATEFUL（transcript+profile 的 KV） | PG 会话 | ✅ |
| RECOVERABLE / PAUSABLE / SCALABLE | restate 同源 | ✅ |
| CONCURRENT（并行工具 + 共享沙箱连接） | 工具串行 | ⭐ 值得学习（后置——重放确定性） |

### 19 特性表对照（值得学习的新增项）

| 官方特性 | Chronotope | 判定 |
|---|---|---|
| Queueing/steering/interruption | A4 + cancel | ✅ 部分 |
| **Parallel tools** | 串行 | ⭐ 后置（④） |
| **Background operations（pending tools）**——审批/定时挂起时**模型继续**、稍后等/取消 | awaitApproval **阻塞模型** | ⭐ 值得学习（⑤——与调研①超时降级同族：审批不阻塞模型） |
| Subagents（follow-up 到同 agent） | child run | ✅ |
| Guardrails / Human approval | class 分级 + awakeable | ✅ |
| **Programmatic tool calling（PTC）**——模型写 JS 组合工具、中间数据不进 context | 无 | ⭐ 后置（⑥——新颖形态） |
| **Tool search**——工具 schema 按需加载 | 全量加载 | ⭐ 后置（⑦） |
| Memory / Compaction / Schedules / Sandboxes | 分层记忆/consolidation/定时/E2B | ✅ |
| **Client updates**——long-poll + revision tags + offset 重连 | SSE + offset | ✅ 部分（revision 增量待补） |
| Extensible tools（MCP + handlers as tools） | MCP | ✅ |
| **Output recovery**——truncated 重试一次更大输出预算 | truncated 落账（无恢复） | ⭐ 值得学习（⑧） |

### 值得学习的完整清单（合并两次调研——按序）

```
① 审批超时 + 降级路径（SLA——orTimeout 语义）
② LLM 重试上限（maxRetryAttempts 成本纪律）
③ 工具错误 → 模型决策（默认路由）
⑤ Background operations（审批/定时挂起时模型继续——pending tools
   ——「审批不阻塞模型」是官方参考架构的核心语义）
⑧ Output recovery（truncated 重试一次更大预算）
④/⑥/⑦ 后置（并行工具/PTC/tool search——重放确定性与 context 预算）
+ interrupt 的「总结已完成」语义 + client updates 的 revision 增量
