# chaos 测试套件（W3 持久性三件套 · 崩溃恢复矩阵）

> 范围（mvp-落地方案 §4 W3 / §6）：kill -9 矩阵 + SSE 断线重连 + Restate 引擎重启。
> 验收标准：重启后自动续跑，且日志证明「不重调 harness、不重跑沙箱」。

## 已落地（5 个自包含场景，共享 test/chaos/lib.sh）

| 脚本 | 注入 | 核心断言（实测全过） |
|---|---|---|
| `kill9-worker.sh` | worker 执行中 kill -9 | 6 项：自动续跑 steps=4；**harness /runs=4（已完成 step 零重调）**；execute=4（在途 step 幂等重发恰好一次） |
| `kill9-harness.sh` | harness 内联工具执行中 kill -9 | 3 项：无状态重启即愈；**同 (run_id, step0) 幂等重发恰好一次**（step0×2） |
| `kill9-executor.sh` | executor 执行中 kill -9 | 5 项：自动续跑；**沙箱按 sandbox_id 存活不重建**（唯一 id=1）；execute=4（在途重发一次） |
| `kill9-api.sh` | api 执行中 kill -9 | 6 项：**SSE 断线按 after=seq 重连续读**（时间轴无缺口、seq 无重复）；worker 侧零重调零重跑（/runs=4、execute=3） |
| `kill9-restate.sh` | Restate 引擎 `docker restart` | 4 项：**invocation 日志持久化于数据卷**，重启透明恢复；在途 step 重发恰好一次 |

kill9-api 曾实证一个架构缺口并已修复：run 状态行原先只由 api 更新（api 崩溃后
runs.status 永远停在 running）→ 现由 **worker 记账 run 终态**（事件才是真相，
状态行是投影；见 internal/restate/run_workflow.go 与单测断言）。

## 运行

```bash
# 前置：make build；compose 仅需 postgres/restate（make dev-up 或 docker compose up -d postgres restate）
# 脚本自托管本地 api/worker/executor/harness（会 stop compose 应用容器，避免端口冲突）
bash test/chaos/kill9-worker.sh
bash test/chaos/kill9-harness.sh
bash test/chaos/kill9-executor.sh
bash test/chaos/kill9-api.sh
bash test/chaos/kill9-restate.sh
```

## 矩阵后置（TODO）

- 执行前/执行后时序注入（当前均为执行中）
- 组合 kill（worker+executor 同杀、全栈重启）
- 容器形态 chaos（docker kill）进 CI（专用 runner）
- 「exec 完成但响应丢失 → 幂等缓存保命窗口」的实弹演示（单测已覆盖
  `TestServerExecuteIdempotentReplay`，chaos 时序落在执行中）

## 维护注意

- harness 脚本按 (run_id, step) + 请求内调用序索引（确定性/重放安全）；`runs start`
  日志行是 /runs 重发计数依据；日志采用 stop 清空 + start 追加（跨重启累计）。
- `uv run` 是包装进程：杀 harness 必须 `pkill -9 -f 'uvicorn app.main:app'`（父+子）。
