# Chronotope

[![CI](https://github.com/BingdiLotus/chronotope/actions/workflows/ci.yml/badge.svg)](https://github.com/BingdiLotus/chronotope/actions/workflows/ci.yml)

> **时空可组合持久运行时** — An agent runtime that persists. Any time, any space, composably.

Chronotope（chrono 时间 + tope 空间）是一个**基础设施底座的 Agent 持久运行时**：以「只追加事件日志 + journal 重放」为地基，以「可插拔 Executor 协议」为空间边界，以「会话生命周期（创建→任务→休眠→唤醒→续跑）」为主轴。harness、模型、沙箱、记忆四者可替换；业务语义（计费/审批）经**策略缝**注入，默认无业务 = 无限/全过。

## 快速开始

```bash
git clone git@github.com:BingdiLotus/chronotope.git && cd chronotope
cp .env.example .env                              # fake 模式无需真实模型密钥
docker compose --env-file .env -f deploy/docker-compose.yml up -d --build
bash scripts/demo.sh                              # 20 阶段全链路演示（一条命令）
bash scripts/ci.sh --e2e                          # 本地 CI 门禁（与 GitHub Actions 同源）
```

架构与路线：`docs/正式版架构.md`（三期规划）、`docs/期2-实施进度.md` / `docs/期3-实施进度.md`
（进度跟踪）、`mvp-落地方案.md`（架构决策）、`边界语义设计.md`（产品语义）、`docs/contracts/`（契约规范）。

## 项目状态：期 1 / 2 / 3 全部落地 ✅

| 期 | 内容 | 验收 |
|---|---|---|
| **MVP（W1–W8）** | 对话闭环、沙箱、持久三件套（HITL/定时唤醒/崩溃恢复）、计量/控制台、分层记忆/风险分级/预算冻结、子 Agent、群聊多 Agent、outbox 交付/投递 | demo 阶段 1–14 |
| **期 1 正确性治理** | 结果重放协议、执行后写窗口（prepared/done 状态机）、审批摘要精确绑定+审计、快照含卷恢复、接纳屏障重投、⑧冻结快照、⑨ComputeLease、⑩spec digest+config 快照、journal 审计导出 | 单测 + chaos + e2e + CI |
| **期 2 时空深化** | 时间旅行（checkpoint 树/fork/diff/rollback + 血缘）、工作区 blob 合同（RustFS 内容寻址 + 第二条恢复链）、冷层归档 + usage 增量 rollup、控制台时空视图（checkpoint 标记/fork/rollback/diff/血缘面包屑/run 树） | w10–w12 + Playwright |
| **期 3 治理与生态** | 三层分离（层 0 原语/层 1 策略缝/层 2 参考业务层）：BudgetPolicy + principal 化、ApprovalRouter + 审批策略（TTL 过期自动拒绝）、MCP 网关 allowlist（默认全拒）、tenant 级共享知识库（pgvector 嵌入检索+跨会话挂载） | w13 7 断言 + 单测矩阵 |

**基础设施底座三层分离**（`docs/期3-细化落地方案.md` §0）：

```
层 0 原语：tenant / principal / 保护性限流 / 中性计量 / 运行态原语（冻结槽）——无业务语义
  ▲ 层 1 策略缝：BudgetPolicy.Check → {Allow|Hold} · ApprovalRouter.Route——默认无限/全过
  ▲ 层 2 参考业务层：OrgDailyBudget / OrgApprovalPolicy——自带示范，可整体替换
```

## 测试体系（三层防御）

| 层 | 命令 | 规模 |
|---|---|---|
| **fake CI** | `bash scripts/ci.sh --e2e` | 单测 + vet + harness pytest 25 + demo **20 阶段** |
| **控制台 e2e** | `bash scripts/console-e2e.sh <console\|tt\|runtree>` | Playwright 5 用例 |
| **真实模型** | `bash scripts/real-e2e.sh`（需 `.env` 密钥） | **9 组 36 断言**全特性 |
| **缺陷回归** | demo 阶段 19（w14）+ pytest | 8 项 fake 绿真实红缺口固化 |

纪律：**fake 绿真实红**——真实 e2e 曾挖出 8 项单测/fake 测不可见的缺陷（快照双缺陷、
fork 初始化、流异常僵尸、tool_call_id 配对等），全部修复并固化为 w14 回归断言。

## 已交付能力全景

- **W1–W8**：对话闭环 / 沙箱（受限容器+幂等缓存+快照含卷恢复）/ HITL 审批（digest 精确绑定）/
  定时唤醒 / kill-9 恢复 / 三轴计量 + Next.js 控制台 / 分层记忆（主题摘要+检索）/ 工具风险分级
  （class 2 强制审批）/ org 预算冻结解冻 / 子 Agent / 群聊多 Agent（moderator 主持）/
  outbox 交付投递
- **期 2**：checkpoint 树 + fork/diff/rollback（事件轴真相不可变）/ 工作区 blob 合同 /
  冷层归档（RustFS ndjson.gz + age 闸门）/ 控制台时空视图 + run 树拓扑
- **期 3**：principal（users/key 绑 user/user 限流）/ BudgetPolicy・ApprovalRouter 策略缝 /
  审批策略（approver 集合 + TTL 过期自动拒绝）/ MCP 网关 allowlist / tenant 共享知识
  （pgvector 余弦检索 + 跨会话挂载注入）/ 审计导出（approval/mcp）
- **运维面**：沙箱生命周期闭环（TTL+lease+GC+启动 sweep+销毁连卷——回归后孤儿 0）/
  org 审计导出 / journal 审计导出（重放轨迹+dedupe 证据链）

## 快速开始（开发模式：宿主机跑三个 Go 二进制）

```bash
# 前置：Go 1.25+（brew install go）、uv、docker compose
make dev-up              # 启动基础设施：postgres / restate / rustfs / litellm
make build               # 构建三个 Go 二进制到 bin/

# 终端 A：worker（Restate 端点）
export DATABASE_URL='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'
./bin/chronotope-worker -addr :9080 -harness-url http://localhost:8000

# 终端 B：api（HTTP/SSE 网关）
export DATABASE_URL='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'
./bin/chronotope-api -addr :8080 -restate-url http://localhost:8081

# 终端 C：harness（fake 模型流，无密钥演示；真实模型去掉 HARNESS_FAKE_MODEL 并配 LITELLM_API_KEY）
cd harness && uv sync && HARNESS_FAKE_MODEL=1 uv run uvicorn app.main:app --port 8000

# 终端 D：注册 worker + 跑 W1 闭环 e2e
make register-worker
make e2e

# 手动闭环：
curl -X POST localhost:8080/orgs/org-demo/agents -H 'content-type: application/json' \
  -d '{"name":"demo","config":{"model":"claude-sonnet-4-6","instructions":"你是演示助手。","tools":[],"version":1}}'
# → 取 agent id 建 session → 取 session id 提交任务（Idempotency-Key 必带）→ SSE 收事件
```

一键容器化：`HARNESS_FAKE_MODEL=1 docker compose -f deploy/docker-compose.yml up -d --build`
（真实模型：在 `.env` 配 `OPENAI_API_KEY`/`ANTHROPIC_API_KEY` 后 `make up`；
切换在 LiteLLM 网关配置化完成，0 代码改动）。

控制台：`cd web && pnpm install && pnpm dev` → http://localhost:3000（会话列表 /
时间轴回放 + checkpoint 标记 / 人控审批 / 时空视图 / run 树 / 记忆 / 三轴计量）。

## 仓库结构

```
chronotope/                     # Go module：github.com/bingdilotus/chronotope（go ≥ 1.25）
├─ cmd/
│  ├─ api/                      # HTTP/SSE 网关（Session API + SSE 时间轴 + 归档/审计/知识）
│  ├─ worker/                   # Restate 端点（五服务 + 策略缝装配）
│  └─ executor/                 # 沙箱编排（docker/E2B 双 driver + 快照 + boot sweep/GC）
├─ internal/
│  ├─ api/                      # 网关实现（REST + SSE hub/poller + ingress + 限流/认证）
│  ├─ core/                     # 共享契约：事件 schema、/runs 协议、Session API 类型
│  ├─ store/                    # Postgres 访问层 + migrations 001–018（幂等，启动自动迁移）
│  ├─ events/                   # 事件投影：SSE hub、outbox 投递
│  ├─ restate/                  # worker 服务层：五 Restate 服务、agent 主循环、策略缝装配
│  ├─ execproto/                # execute 协议：Driver 接口（docker / e2b）
│  ├─ policy/                   # 策略缝：BudgetPolicy / ApprovalRouter + 参考实现
│  └─ blobstore/                # RustFS S3 门面（归档 + 工作区内容寻址）
├─ spike/                       # W1 D1 Restate Go SDK 五项原语 spike（独立 module）
├─ harness/                     # Python 服务（uv + FastAPI）：POST /runs → SSE · /embed
├─ web/                         # Next.js 控制台（compose profile "web"）
├─ deploy/                      # docker-compose.yml + Dockerfile×5 + litellm 配置
├─ scripts/                     # demo.sh（20 阶段）/ ci.sh / console-e2e.sh / real-e2e.sh
├─ docs/
│  ├─ contracts/                # 三协议契约定义 + events.schema（与契约测试同源）
│  └─ 期2-*.md / 期3-*.md       # 细化方案 + 实施进度（含审计/复审记录）
└─ test/
   ├─ e2e/                      # w1–w14 + real-*（9 组真实模型）
   ├─ contract/                 # 契约测试
   └─ chaos/                    # kill -9 恢复验证
```

## 文档索引

| 文档 | 内容 |
|---|---|
| [正式版架构.md](./docs/正式版架构.md) | 三期规划与风险清单（期 1/2/3 全落地） |
| [期2-实施进度.md](./docs/期2-实施进度.md) | 期 2 交付记录（时间旅行/blob/归档/时空视图） |
| [期3-实施进度.md](./docs/期3-实施进度.md) | 期 3 交付记录（策略缝/审批/MCP/知识 + 审计复审） |
| [期3-细化落地方案.md](./docs/期3-细化落地方案.md) | 三层分离设计（基础设施底座原则） |
| [agent-saas-持久执行调研报告.md](./agent-saas-持久执行调研报告.md) | 市场调研：托管平台 / 持久执行引擎 / 沙箱 / 调度 |
| [mvp-技术选型.md](./mvp-技术选型.md) | 技术选型（**现行方案见 §8**） |
| [mvp-落地方案.md](./mvp-落地方案.md) | 落地计划：W1–W8 里程碑 + 设计附录 |
| [worker-架构设计.md](./worker-架构设计.md) | worker 内部架构（五服务、状态模型、故障矩阵） |
| [契约规范.md](./契约规范.md) | 契约权威定义：三协议、事件全集、错误码、兼容性 |
| [边界语义设计.md](./边界语义设计.md) | 熔断 / 权限 / 截断 / 删除 / 时钟 / 准入 + 分层记忆 |

## 命名规范

```
Go module    github.com/bingdilotus/chronotope
二进制       chronotope-api · chronotope-worker · chronotope-executor
Python 服务  chronotope-harness
镜像         chronotope/api · /worker · /executor · /harness · /web
存储         RustFS（S3 兼容）· Postgres（pgvector）
中文副标题   时空可组合持久运行时
```

## 核心架构一句话

```
用户 → api(Go) → worker(Go, Restate 端点) ⇄ Restate 引擎
                      │ journal 重放 · 输出缓存 · awakeable · 策略缝（Budget/Approval）
                      ├─ POST /runs → SSE → harness(Python, 无状态) → LiteLLM → 模型
                      └─ execute 协议 → executor(Go) → 沙箱（Docker / E2B 自托管）
事件 append-only → Postgres（时间轴/审计/计量/知识向量）· 工件与归档 → RustFS
```

**三条纪律**：harness 无状态；事件 append-only；重放不重调（缓存键 = journal 位置，版本绑定在 run）。

## 后置（不在当前三期）

- PG 分区 parquet 冷层（当前 ndjson.gz 归档已够；量大才痛）
- E2B 真实实例 smoke（凭证就绪即跑 `test/e2e/e2b-smoke.sh`）
- 真实模型夜间回归 CI job（repo secrets 配代理 key 后可开 `workflow_dispatch`）
- 嵌入模型凭据（共享知识当前回退确定性向量——写入/查询一致全链可测；配 embedding
  端点后即真实向量）
