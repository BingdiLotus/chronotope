# chaos 测试套件（W3 持久性三件套 · 崩溃恢复）

> 范围（mvp-落地方案 §4 W3 / §6）：kill -9 矩阵 + SSE 断线重连 + Restate 引擎重启。
> 验收标准：重启后自动续跑，且日志证明「不重调 harness、不重跑沙箱」。

## 已落地：kill9-worker.sh（自包含）

```bash
bash test/chaos/kill9-worker.sh [API_URL]
```

场景：harness 脚本模式产出 3 步 bash（各 sleep 2s）+ 终答；第 1 步完成（journaled）、
第 2 步 exec 执行中 **kill -9 worker** → 重启（同地址，部署注册不失效）→ 自动续跑。

验证（6 项断言，实测全过）：

| 断言 | 结果语义 |
|---|---|
| 崩溃后自动续跑完成（steps=4） | Restate 重放到断点 |
| sandbox.exec ×3 / tool.call ×3（不重复发射） | 事件 dedupe 键幂等吞重放 |
| harness `/runs` 增量 = 4 | **已完成 step 零重调**（journal 缓存回放） |
| executor execute 增量 = 4（3 步 + 在途重发 1 次） | 断点 step 幂等重发恰好一次（§9.4 崩溃矩阵） |

「已完成但响应丢失」窗口由 executor 幂等缓存兜底（sandbox_execs，绝不复跑命令——
见 execproto 单测 TestServerExecuteIdempotentReplay）。

## 矩阵与后置（TODO）

| 挂掉谁 | 预期行为（§9.4） | 状态 |
|---|---|---|
| worker | Restate 重放到断点；已完成 step 走缓存 | ✅ kill9-worker.sh |
| harness | worker 按重试策略重发同一 (run_id, step) | TODO：kill9-harness.sh（无状态重启即愈，同理验证） |
| executor | 重发 execute；沙箱按 sandbox_id 恢复或重建 | TODO：kill9-executor.sh |
| api | 无状态；SSE after=seq 续读 | TODO（SSE 断线重连已由 w1-loop 覆盖） |
| 组合 kill + Restate 引擎重启 | 全栈重启各自恢复 | TODO |
| CI 集成 | 容器形态 kill（docker kill） | TODO（专用 runner） |

## 维护注意

- harness 脚本按 run 内 step 索引（确定性/重放安全）；executor 日志 `INFO execute`
  行是沙箱执行计数依据；脚本自托管 worker/harness/executor 生命周期，api 需保持运行。
