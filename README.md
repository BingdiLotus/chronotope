# Chronotope

> **时空可组合持久运行时** — An agent runtime that persists. Any time, any space, composably.

Chronotope（chrono 时间 + tope 空间）是一个可在任意时间、任意空间持久执行的 Agent SaaS 运行时：以「只追加事件日志 + journal 重放」为地基，以「可插拔 Executor 协议」为空间边界，以「会话生命周期（创建→任务→休眠→唤醒→续跑）」为主轴；harness、模型、沙箱、记忆四者可替换。

## 项目状态

**MVP 核心闭环（W1–W4）已完成** ✅——创建 Agent → 持久 Session → 提交任务 → 崩溃恢复 →
定时/事件唤醒 → 沙箱执行 → 时间轴回看 → 三轴计量。周度验收全部有可重放 e2e：
W1 对话闭环（11 项断言）、W2 沙箱闭环（9 项）、W3 持久性三件套（HITL 4 项 + 定时唤醒 5 项 +
kill -9 崩溃恢复 6 项）、W4 计量三轴 + 控制台（会话列表/时间轴回放/用量）。

已落地：store 数据访问层（幂等事件/消息/沙箱事实状态/exec 幂等缓存/schedules/usage）、
worker 四个 Restate 服务（session_object / run_workflow agent 主循环 journal 缓存 /
scheduler 定时唤醒 / webhook HITL）+ 四类工具分流、api Session API 网关（REST + SSE 时间轴
after=seq 续读 + 审批回调 + actions + 计量）、executor docker driver（受限容器 + 幂等缓存）、
harness agent 循环（LiteLLM 网关流式 + fake 脚本模式）、Next.js 控制台。

**DoD 六条全部达成**（落地方案 §8）：① 一条 `bash scripts/demo.sh` 复现全部场景
（实测 6/6）② 三个持久性 demo 可重放脚本 ③ 时间轴回看 ④ 计量三轴 ⑤ 契约测试
⑥ 边界语义契约测试（终止帧/截断、tombstone、限流 429 + Retry-After、双开 409、
无进展检测、run 级预算熔断）。本地 CI 门禁 `bash scripts/ci.sh [--e2e]` 全绿。

**已交付能力全景**：W1 对话闭环 → W2 沙箱 → W3 持久三件套（HITL/定时唤醒/
崩溃恢复）→ W4 计量/控制台 → W5 分层记忆/风险分级/预算冻结 → W6 子 Agent →
W7 群聊多 Agent → W8 会话导出 tar + 孤儿沙箱 GC。demo 一条命令复现 11 阶段
（50 项断言）；本地 CI 全绿。

**正确性闭合（评审驱动修复，2026-10）**：结果重放协议（缓存命中 SSE 化 +
终止帧强制）、执行后写窗口（prepared/done 状态机 + 输入摘要 + in-flight
停派发）、审批动作摘要精确绑定 + 审批者审计、快照含卷恢复（named volume
显式打包）+ effect 账本与沙箱生命周期解耦、接纳屏障重投扫描 + 非抢占式
取消。全部经单测 + e2e + 本地 CI 回归（demo 15 阶段 67 项断言）。

**后置迭代**：PG 分区 parquet 冷层归档与 usage 增量 rollup（MVP 事件量下
收益低——单会话 <1 万行时全量导出/重建已足够；量大才痛）、ComputeLease
释放合同（沙箱资源按依赖安全释放，替代 sleep-infinity + TTL GC）、
E2B 真实实例 smoke（凭证就绪即跑 e2b-smoke.sh）、GitHub CI 实跑。真实模型
调用待配 `OPENAI_API_KEY`/`ANTHROPIC_API_KEY`（代码已就绪）。

## 快速开始（W1 闭环）

```bash
# 前置：Go 1.25+（brew install go）、uv、docker compose
make dev-up              # 启动基础设施：postgres / restate / minio / litellm
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
可经 `OPENAI_BASE_URL`/`ANTHROPIC_BASE_URL` 指向官方端点或任意 OpenAI/Anthropic
兼容代理——切换在 LiteLLM 网关配置化完成，0 代码改动）。

**本地 CI 门禁**：`bash scripts/ci.sh`（gofmt/build/vet/test 含 store 集成与
golden journal 重放 → harness → spike），`bash scripts/ci.sh --e2e` 追加 compose
全链路（.github/workflows/ci.yml 的本地等价物，推送前建议先跑）。

**全场景演示（W1–W4 一条命令复现，实测 6/6 通过）**：`bash scripts/demo.sh`
——全栈启动 → 注册 worker → W1 对话闭环（11 项断言）→ W2 沙箱闭环（9 项）→
W3 HITL（4 项）→ W3 定时唤醒（5 项）。
控制台：`cd web && pnpm install && pnpm dev` → http://localhost:3000（会话列表 /
时间轴回放 / 三轴计量）。

## 仓库结构（落地方案 §1）

```
chronotope/                     # Go module：github.com/bingdilotus/chronotope（go ≥ 1.25）
├─ go.work                      # 预留多模块工作区（当前单模块，见文件内注释）
├─ cmd/
│  ├─ api/                      # HTTP/SSE 网关（Session API + SSE 时间轴）
│  ├─ worker/                   # Restate 端点（session_object/run_workflow/scheduler/webhook）
│  └─ executor/                 # 沙箱编排服务（executor 协议路由骨架，W2 接 docker driver）
├─ internal/
│  ├─ api/                      # Session API 网关实现（REST + SSE hub/poller + ingress 控制面）
│  ├─ core/                     # 共享契约：事件 schema、/runs 协议、Session API 类型
│  ├─ store/                    # Postgres 访问层 + migrations（events/sessions/usage/outbox…）
│  ├─ events/                   # 事件投影：SSE hub（提示+回查、水位）、outbox 投递
│  ├─ restate/                  # worker 服务层：四 Restate 服务、agent 主循环、harness 客户端、journal helper
│  └─ execproto/                # execute 协议：Driver 接口（docker / e2b）
├─ spike/                       # W1 D1 Restate Go SDK 五项原语 spike（独立 module）
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
