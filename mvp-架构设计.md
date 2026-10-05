# MVP 架构设计（v3：Go 平台 + 借 harness）

> 项目：**Chronotope（时空可组合持久运行时）**
> 配套：[mvp-技术选型.md](./mvp-技术选型.md) · 渲染图：[mvp-架构图.png](./mvp-架构图.png)（源文件 [mvp-架构图.svg](./mvp-架构图.svg)）
> 本文档含可编辑的 Mermaid 图：容器图 + 三条关键流程时序图 + 部署视图。

## 1. 容器图

```mermaid
flowchart LR
  subgraph 客户端
    U[用户/开发者<br/>SDK · CLI · REST]
    W1[控制台 Web · Next.js<br/>时间轴回放 · 会话 · 用量]
  end

  subgraph Go平台[Go 平台 · 护城河]
    API[api · Go<br/>REST+SSE · Agent/Session · 事件投影<br/>计量 · outbox · 幂等]
    WORKER[worker · Go<br/>session_object · run_workflow<br/>scheduler cron · webhook HITL]
    EXEC[executor · Go<br/>execute name,input 协议<br/>工具路由 · 沙箱编排]
  end

  subgraph 持久核心
    RESTATE[(Restate 引擎<br/>事件日志 · 重放 · 输出缓存<br/>durable timer · awakeable)]
  end

  subgraph 智能层[智能层 · Python 借用]
    HARNESS[harness · Python 无状态<br/>OpenAI Agents SDK]
    LITELLM[LiteLLM 网关<br/>多模型路由/密钥]
  end

  subgraph 数据
    PG[(Postgres 16<br/>events append-only · sessions<br/>usage · outbox · pgvector)]
    MINIO[(MinIO S3<br/>工件 · 快照)]
  end

  subgraph 执行空间
    SB[沙箱 自托管<br/>dev: Docker executor<br/>prod: E2B Firecracker]
  end

  subgraph 外部
    MODELS[模型厂商<br/>OpenAI 兼容 · Anthropic]
    EVT[外部事件<br/>Webhook · HITL 审批]
  end

  U -->|建 Agent/Session · 提交任务| API
  W1 -->|SSE 时间轴| API
  API -->|run 入 queued 态 + NOTIFY（拉模式）| PG
  API -->|元数据 · 用量聚合| PG
  WORKER <-->|journal / state| RESTATE
  WORKER -->|POST /runs → SSE| HARNESS
  HARNESS -->|OpenAI 协议| LITELLM
  LITELLM --> MODELS
  WORKER -->|代码/命令工具| EXEC
  EXEC -->|容器/微VM 生命周期| SB
  WORKER -->|事件 append-only（唯一写入者）| PG
  EXEC -->|工件| MINIO
  EVT -->|webhook 唤醒 · resolve awakeable| API
```

**三条纪律**：① harness 无状态（Session 状态 100% 在 Go 平台持久层）；② harness 调用输出以 **journal 位置（run_id+step）** 为缓存键，模型/协议版本由 run 绑定承载（见契约规范 §1 权威定义），重放不重调；③ 平台只认 `POST /runs` 一条协议，harness 可替换。

## 2. 关键流程时序图

### 2.1 提交任务与持久执行（happy path）

```mermaid
sequenceDiagram
  autonumber
  participant U as 用户/SDK
  participant API as api (Go)
  participant W as worker (Go)
  participant R as Restate
  participant H as harness (Python)
  participant L as LiteLLM
  participant M as 模型
  participant E as executor (Go)
  participant S as 沙箱
  participant PG as Postgres

  U->>API: 创建 Agent / Session
  API->>PG: session 元数据
  U->>API: 提交任务（幂等键）
  API->>PG: run 入 queued 态 + NOTIFY
  PG-->>W: 通知拉取（拉模式）
  W->>W: run_workflow.start(run_id)
  loop agent loop（每轮 = 1 个 journaled step）
    W->>H: POST /runs（会话消息 + 工具清单）
    H->>L: 模型调用
    L->>M: OpenAI 兼容 / Anthropic
    M-->>L: 流式响应
    L-->>H: 流式透传
    H-->>W: SSE：回复 + tool_calls
    W->>R: journal 输出（缓存键 run_id+step+model版本）
    alt 需执行代码/命令
      W->>E: execute(name, input)
      E->>S: 容器/微VM 执行
      S-->>E: 结果
      E-->>W: 结果
      W->>R: journal 结果
    end
  end
  W->>PG: append 事件（时间轴投影）
  W-->>API: run 完成
  API-->>U: SSE 完成事件
```

### 2.2 崩溃恢复（持久执行核心演示）

```mermaid
sequenceDiagram
  participant W as worker (Go)
  participant R as Restate
  participant H as harness
  participant E as executor

  Note over W,R: run 执行到 step3 时 kill -9 worker 进程
  W--xW: 进程被杀
  Note over R: 重启后检测到未完成 run
  R->>W: 重放 run 至断点
  W->>R: 读取 step1/step2 缓存（harness 输出 / 沙箱结果）
  Note over W,H: 不重调 harness、不重调模型、不重跑沙箱
  W->>E: 继续 step3（幂等）
  E-->>W: 结果
  W->>R: journal 完成
```

### 2.3 任意时间：定时唤醒 + HITL 审批

```mermaid
sequenceDiagram
  participant S as scheduler (worker)
  participant R as Restate
  participant W as worker
  participant API as api
  participant X as 外部（审批人/Webhook）

  Note over S,R: cron 到点（durable timer，跨重启存活）
  S->>R: timer 触发 → wake session
  R->>W: session_object.wake()
  W->>W: 从持久层恢复会话状态 → 提交新一轮 run
  W->>R: 创建 awakeable（挂起等待审批）
  W-->>API: 事件：需要人工审批
  API-->>X: outbox webhook / SSE 提示
  X->>API: 审批结果回调
  API->>W: resolve awakeable(id, 结果)
  W->>W: 继续执行（worker 可能已重启，状态从 Restate 恢复）
```

## 3. 组件职责表

| 组件 | 语言/形态 | 职责 | MVP 边界内做什么 |
|---|---|---|---|
| api | Go 二进制 | REST/SSE 网关；Agent/Session 管理；事件投影；计量；outbox；幂等 | 全做 |
| worker | Go 二进制（Restate 端点） | session_object（会话状态机）；run_workflow（任务编排 + journal 缓存）；scheduler（cron）；webhook（HITL） | 全做 |
| Restate | 单二进制引擎 | 事件日志、重放、durable timer、awakeable、重试策略 | 部署+配置，不改代码 |
| harness | Python 服务（无状态） | agent loop：POST /runs → SSE；API 类工具执行；经 LiteLLM 调模型 | 全做（SDK 现成代码） |
| LiteLLM | Docker | 多模型路由/密钥/限流/用量 | 配置化，不改代码 |
| executor | Go 二进制 | execute 协议实现：Docker 容器（dev）、E2B 自托管（prod）；capability 路由 | dev 档全做，prod 档 W4 验证 |
| Postgres | Docker | events（append-only）、sessions、usage、outbox、pgvector | 全做 |
| MinIO | Docker | 工件/文件/快照 | 全做 |
| 控制台 Web | Next.js | 时间轴回放、会话管理、用量页 | 最小可用 |
| 沙箱 | Docker/Firecracker | 不可信代码执行 | dev: Docker executor |

## 4. 部署视图（docker compose，单机自托管）

```mermaid
flowchart TB
  subgraph host["一台机器（dev: 任意；prod 沙箱加 Linux+KVM 主机）"]
    direction LR
    BIN1[api 二进制]
    BIN2[worker 二进制]
    BIN3[executor 二进制]
    C1[restate 容器]
    C2[postgres 容器]
    C3[minio 容器]
    C4[litellm 容器]
    C5[harness 容器 · FastAPI]
    C6[web 容器 · Next.js]
  end
  外部1[模型厂商 API] <-->|出站| C4
  外部2[用户浏览器/SDK] <-->|入站 8080| BIN1
  外部3[审批 Webhook] -->|入站| BIN1
```

- 3 个 Go 二进制交叉编译、随镜像分发；harness 一个 Python 镜像。
- 出站网络：模型厂商（LiteLLM）、沙箱执行 egress 白名单；入站：api 8080 与 webhook 回调。
- 生产演进：executor 拆到 Linux(KVM) 主机跑 E2B 自托管，其余不变。

## 5. 数据流要点

1. **事件是单一真相**：所有状态变更 append-only 写 `events(session_id, seq, type, payload)`——审计、时间轴回放、计量三用。
2. **双通道输出**：在线 SSE（实时 UI）、离线 outbox webhook（无人值守）消费同一事件流投影。
3. **重放缓存规则**：`run_id + step + provider/model版本` → harness 输出；沙箱结果同样 journal。重放永不重调模型/沙箱。
4. **凭证边界**：secrets 只在 LiteLLM 与 executor 注入层存在，不进事件日志、不进 harness 进程内存之外。
5. **harness 无状态**：崩溃后重启即空，会话重建信息全部来自 `POST /runs` 入参（由 worker 从持久层组装）。
