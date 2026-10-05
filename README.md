# Chronotope

> **时空可组合持久运行时** — An agent runtime that persists. Any time, any space, composably.

Chronotope（chrono 时间 + tope 空间）是一个可在任意时间、任意空间持久执行的 Agent SaaS 运行时：以「只追加事件日志 + journal 重放」为地基，以「可插拔 Executor 协议」为空间边界，以「会话生命周期（创建→任务→休眠→唤醒→续跑）」为主轴；harness、模型、沙箱、记忆四者可替换。

## 项目状态

**W1 进行中**：仓库骨架已初始化（Go 三二进制 + harness 四服务骨架、compose、三契约落库、契约测试），下一步为 W1 D1 Spike（Restate Go SDK 五项原语验证）与 D3–D4 三服务实现。

## 快速开始（W1 骨架）

```bash
# 前置：Go 1.23+（brew install go）、uv、docker compose
make dev-up              # 启动基础设施：postgres / restate / minio / litellm
make build               # 构建三个 Go 二进制到 bin/
make contract-test       # Go 契约测试 + harness 侧 /runs 契约测试

# harness 骨架（POST /runs → SSE，done 帧占位；LiteLLM 接入待 W1 D3–D4）
cd harness && uv sync && uv run uvicorn app.main:app --port 8000
curl -N -X POST localhost:8000/runs -H 'content-type: application/json' \
  -d '{"protocol":"1.0","run_id":"r_1","session_id":"s_1","step":0,"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}'
```

## 仓库结构（落地方案 §1）

```
chronotope/                     # Go module：github.com/bingdilotus/chronotope
├─ go.work                      # 预留多模块工作区（当前单模块，见文件内注释）
├─ cmd/
│  ├─ api/                      # HTTP/SSE 网关（Session API 路由骨架）
│  ├─ worker/                   # Restate 服务端点（spike 后接入 SDK）
│  └─ executor/                 # 沙箱编排服务（executor 协议路由骨架）
├─ internal/
│  ├─ core/                     # 共享契约：事件 schema、/runs 协议、Session API 类型
│  ├─ store/                    # Postgres 访问层 + migrations（events/sessions/usage/outbox…）
│  ├─ events/                   # 事件投影：SSE hub、outbox 投递
│  ├─ restate/                  # journal 缓存键 helper、大小策略
│  └─ execproto/                # execute 协议：Driver 接口（docker / e2b）
├─ harness/                     # Python 服务（uv + FastAPI）：POST /runs → SSE
├─ web/                         # Next.js 控制台（W4，compose profile "web"）
├─ deploy/                      # docker-compose.yml + Dockerfile.{api,worker,executor,harness,web} + litellm 配置
├─ docs/contracts/              # 三个契约的正式定义（API-first，与契约测试同源）
└─ test/
   ├─ contract/                 # 契约测试（事件 schema / 缓存键 / 三协议样例）
   └─ chaos/                    # kill -9 恢复验证脚本骨架（W3）
```

## 文档索引

| 文档 | 内容 |
|---|---|
| [agent-saas-持久执行调研报告.md](./agent-saas-持久执行调研报告.md) | 市场调研：托管平台 / 持久执行引擎 / 沙箱 / 调度 |
| [mvp-技术选型.md](./mvp-技术选型.md) | 技术选型（**现行方案见 §8**：Go 平台 + 借 harness） |
| [mvp-架构设计.md](./mvp-架构设计.md) | 容器图 + 三条关键流程时序图 + 部署视图 |
| [mvp-落地方案.md](./mvp-落地方案.md) | 落地计划：W1–W8 里程碑 + 18 个设计附录 |
| [worker-架构设计.md](./worker-架构设计.md) | worker 内部架构（四个 Restate 服务、状态模型、故障矩阵） |
| [契约规范.md](./契约规范.md) | 契约权威定义：三协议、事件全集、错误码、兼容性 |
| [边界语义设计.md](./边界语义设计.md) | 熔断 / 权限 / 截断 / 删除 / 时钟 / 准入 + 分层记忆 |

## 命名规范

```
Go module    github.com/<org>/chronotope
二进制       chronotope-api · chronotope-worker · chronotope-executor
Python 服务  chronotope-harness
镜像         chronotope/api · /worker · /executor · /harness
中文副标题   时空可组合持久运行时（文档标题保留）
```

## 核心架构一句话

```
用户 → api(Go) → worker(Go, Restate 端点) ⇄ Restate 引擎
                      │ journal 重放 · 输出缓存 · awakeable
                      ├─ POST /runs → SSE → harness(Python, 无状态) → LiteLLM → 模型
                      └─ execute 协议 → executor(Go) → 沙箱（Docker/E2B 自托管）
事件 append-only → Postgres（时间轴/审计/计量）· 工件 → MinIO
```

**三条纪律**：harness 无状态；事件 append-only；重放不重调（缓存键 = journal 位置，版本绑定在 run）。
