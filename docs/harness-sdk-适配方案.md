# 第三方 SDK 官方适配方案（Claude Codex / Pi / deepseek-harness 分批实现）

> 2026-10-11 方案定稿（先文档后编码）。目标：三个真实第三方 SDK 经**官方
> 本地 SDK**接入 Chronotope——每个实现表现官方最佳实践，优先本地 SDK（进程
> 内调用）而非远程 API 模拟。
> 调研核实：Claude Agent SDK 有[官方 Python SDK](https://code.claude.com/docs/en/agent-sdk/python)
> （query/hooks/permissions/checkpointing）；Pi 有 [earendil-works/pi](https://github.com/earendil-works/pi)
> （packages/coding-agent 本地 npm 包）；dsh 有 [@deepseek-ai/dsh-sdk-client](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/sdk/client/README.md)
> （官方 SDK client）。

## 一、总体架构（三个适配器同一形态）

```
┌────────────────────────────────────────────────────────┐
│ 官方 SDK 适配器进程（本地 SDK 调用——官方最佳实践）      │
│  ├─ Claude Codex：Python claude-agent-sdk（query+hooks）│
│  ├─ Pi：Node pi-ai（coding-agent 本地包）               │
│  └─ dsh：TypeScript @deepseek-ai/dsh-sdk-client         │
├────────────────────────────────────────────────────────┤
│ /runs 协议适配层（请求/帧映射——与 third-party-harness  │
│ 同形态；SDK 的工具调用 → execute 幂等键；审批 → HITL；  │
│ checkpoint → 快照；usage → 账本）                       │
└────────────────────────────────────────────────────────┘
        ↓ /runs 协议（0 平台改动）
Chronotope：effect ledger / HITL digest / 审计链 / 快照路由
```

**关键原则**：SDK 是「执行器」不是「harness 的替身」——SDK 的本地能力
（hooks/permissions/checkpointing）映射到 Chronotope 的对应语义（class
分级/审批/快照），**双份语义取其严**（SDK 的权限与 Chronotope 的 class
分级都要过——不互相豁免）。

## 二、官方最佳实践映射表（每个适配器遵守）

| SDK 官方能力 | Chronotope 对应 | 映射语义 |
|---|---|---|
| Claude hooks（PreToolUse/PostToolUse） | class 分级 + HITL digest | hook 拦截 → class 2 的审批绑定动作摘要 |
| Claude permissions（canUseTool） | 工具 allowlist + risk class | 权限判定取 SDK 与平台的双重结果 |
| Claude checkpointing（file rewind） | checkpoints（时间旅行） | SDK 的 checkpoint → Chronotope 快照坐标 |
| Claude usage/cost | effect ledger（tokens/usage） | SDK 的 usage → llm_calls 账本行 |
| Pi 的 harness 循环 | run_workflow 主循环 | Pi 的 turn → Chronotope 的 step |
| Pi 的 ownership/子 agent | 子 Agent（W6） | Pi 的 foreground/background → child run |
| dsh 的 Seam/capabilities | executor Seam 接缝 | dsh 的 Seam → Chronotope 的 executor 协议 |
| dsh 的 Cordis 可逆效应 | effect ledger（可补偿层） | 进程内可逆 vs 进程外可补偿的分层 |

## 三、分批实现（每批的验收）

### 批 1：Claude Codex（Python——官方 claude-agent-sdk）

1. **官方最小示例跑通**：claude-agent-sdk 的 `query()` 本地调用（官方
   quickstart 的形态——不魔改）
2. **适配器**：`adapters/claude-codex/harness.py`——/runs 请求 → SDK
   query 的 messages 映射；SDK 的 result → SSE 帧（delta/done）；SDK 的
   tool_use → tool_call 帧（execute 幂等键）
3. **最佳实践映射**：hooks（PreToolUse 拦 class 2 → 审批请求帧）+
   permissions（工具 allowlist）+ usage（账本）
4. **验收**：w25-claude-codex.sh——真实 SDK 对话 run 完成 + 工具调用走
   execute 幂等键 + 审批绑定 digest + 账本 token 非零

### 批 2：Pi（Node——pi-ai 本地包）

1. **官方最小示例跑通**：pi-ai 的 coding-agent 本地调用（npm 包官方
   README 的形态）
2. **适配器**：`adapters/pi/harness.mjs`——Pi 的 turn/ownership → /runs
   帧；Pi 的子 agent（foreground/background）→ child run 语义
3. **最佳实践映射**：Pi 的 harness 循环 → step；Pi 的 taskGraph →
   WorkItem 队列（M2）
4. **验收**：w26-pi.sh——真实 Pi 对话 + 工具调用 + 子 Agent 派发的闭环

### 批 3：deepseek-harness（TypeScript——@deepseek-ai/dsh-sdk-client）

1. **官方最小示例跑通**：dsh-sdk-client 的官方 README 调用（rc 线注意
   破坏性改动——锁定版本）
2. **适配器**：`adapters/dsh/harness.ts`——dsh 的 client 会话 → /runs
   帧；dsh 的 Seam/capabilities → executor 协议的能力声明
3. **最佳实践映射**：dsh 的 Cordis 可逆效应 → 可补偿层（effect ledger）+
   审计链（不可逆层）——三态谱系的分层落地
4. **验收**：w27-dsh.sh——真实 dsh 调用 + 能力声明 + 幂等键的完整闭环

## 四、每批的共同验收基线

- **conformance**：`go test ./test/contract/`（帧级一致性——适配器的帧
  与契约样例同源）
- **注册表接入**：每个适配器经 harness_registry 注册（官方 SDK 的
  capabilities 声明）——w24 的路径复用
- **e2e 全链路**：真实 SDK 调用 + Chronotope 的账本/审批/快照/审计链
  ——不是「SDK 单独跑通」而是「SDK 在 Chronotope 语义下闭环」
- **官方最佳实践的可证明性**：每个适配器的 README 引用官方文档的对应
  章节（hook/permission/checkpoint 等——不魔改官方 API）

## 五、本地 SDK 优先的具体含义

- **Claude Codex**：本地 `pip install claude-agent-sdk` + 进程内
  `query()`——不调 Claude 的远程 Agent API（本地 SDK 是官方最佳实践）
- **Pi**：本地 `npm install pi-ai`（earendil-works/pi 的 coding-agent
  包）——不拉远程 Pi 服务
- **dsh**：本地 `npm install @deepseek-ai/dsh-sdk-client`（锁定 rc 版本）
  ——dsh 的 client SDK 本地接入
- 模型密钥仍经 litellm/官方 provider（模型调用是 SDK 内部的事——SDK 的
  官方配置方式）

## 六、风险与边界

- **dsh 是 rc 线**（0.1.x-rc——明确破坏性改动）——锁版本 + 每批独立
  验收，不承诺跨版本
- **Pi 的包生态变化快**（2026-08 后兴起）——以官方 README 的当前形态
  为准，验收记录版本
- **SDK 的进程内状态**：SDK 可能自带会话/记忆——**三铁律**（harness
  无状态）的边界：SDK 的内部状态不持久化到 Chronotope（适配器每次 run
  重建 SDK 会话——上下文全部来自 /runs 入参）
- **双份权限的冲突**：SDK 权限与 class 分级冲突时取严——映射表的测试
  覆盖（批 1 的 hooks 拦截反例）

## 七、分阶段落地（批内顺序）

```
每批：① SDK 官方示例跑通（记录版本）→ ② /runs 适配器（请求/帧映射）
→ ③ 最佳实践映射（hooks/permissions/usage 等）→ ④ conformance +
注册表 + e2e（w25/w26/w27）
```
