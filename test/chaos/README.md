# chaos 测试套件（W3 持久性三件套）

> 范围（mvp-落地方案 §4 W3 / §6）：kill -9 矩阵 + SSE 断线重连 + Restate 引擎重启。
> 验收标准：重启后自动续跑，且日志证明「不重调 harness、不重跑沙箱」。

## 矩阵

| 挂掉谁 | 预期行为（落地方案 §9.4 崩溃矩阵） |
|---|---|
| harness | worker 按 Restate 重试策略重发同一 `(run_id, step)` 的 `/runs`（幂等） |
| executor | 重发 `execute`；沙箱按 sandbox_id 恢复或重建 |
| worker | Restate 重放到断点：已完成 step 读缓存，断点 step 继续 |
| api | 无状态；SSE 客户端按 after=seq 重连续读 |

## 脚本

- `kill9.sh <api|worker|executor|harness>`：对 compose 服务执行 kill -9
- `verify-recovery.sh`：TODO(W3)——断言事件序列连续（允许 seq gap）且无重复 harness/exec 调用

## W3 TODO

- 注入时序：run 执行到第 N 步时 kill（需要 run 步进桩），执行前/中/后各一轮
- 组合 kill（worker+executor 同杀等）
- Restate 引擎重启演练
- 记录脚本 + 可重放 demo（验收物）
