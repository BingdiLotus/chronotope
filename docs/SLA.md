# 可靠性 SLA（期 5 §C）

> 状态：目标 + 基线实测（2026-10-08 本机实测；生产基线上线后按同法重测）。
> SLO 监控告警后置（外部可观测系统对接）；本文档是目标定义与基线参照。

## 一、SLA 目标与基线实测

| 指标 | 目标 | 基线实测（本机 compose，fake 模型） |
|---|---|---|
| **run 提交延迟**（POST /runs 响应） | P99 < 1s | **P50=11ms / P99=248ms**（n=20） |
| **SSE 首帧**（events?after= 首帧） | P99 < 1s | **49ms**（urllib 直测；curl 管道下需 -N 行缓冲——15s 假象实证） |
| **PITR 恢复 RTO**（D1 演练） | < 5 分钟 | **< 2 分钟**（隔离容器演练：归档开启 + basebackup + 点时间回卷） |
| **PITR 恢复 RPO** | < 1 分钟 | **≈ 秒级**（pg_switch_wal 强制归档后——目标时刻 WAL 已归档） |
| **Restate 状态恢复 RTO**（D3 演练） | < 5 分钟 | **< 1 分钟**（卷 tar 快照 + 回填 + awakeable resolve 完成） |
| **RustFS 归档回放 RTO**（D2 演练） | < 5 分钟 | **< 30 秒**（SigV4 下载 + ndjson 物化回放 = 基线） |
| **沙箱创建**（E2B 官方云） | 秒级 | 秒级（创建即 ready——运维基准） |

**测量方法**：run/SSE 延迟用 urllib 单进程计时（curl 管道缓冲会失真——49ms
vs 15s 假象实证）；恢复 RTO 取 test/dr 演练脚本实测（dr.yml 每次执行即复测）。

## 二、量级触发预案（期 4 后置项的「触发即执行」）

### parquet 冷层导出
- **触发信号**：RustFS 归档总量 > 50 GB 或归档查询/导出的耗时 > 10s
- **执行步骤**：① 归档对象批量转 parquet（列存：session/run/type/at）；②
  parquet 文件走 RustFS 新前缀 `parquet/`；③ 查询路径切 parquet（会话导出/
  物化回放优先 parquet）
- **验证**：千万级事件 parquet 导出计时 + 回放断言（复用 test/scale 模式）

### 装箱 bin-packing
- **触发信号**：executor 池 ≥ 3 宿主或出现「资源不均衡/单宿主过载」
- **执行步骤**：① SchedulerPolicy 加资源维度候选（ExecutorCandidate 带
  CPU/mem 余量——心跳上报扩展）；② 参考实现 PackPolicy（最小剩余资源优先）；
  ③ 策略缝替换（层 2 业务——成本/亲和约束）
- **验证**：w15 变体（双 executor 不同容量——分配断言按容量比例）

### Restate 集群化
- **触发信号**：可用性要求（单节点停机不可接受）或 invocation 吞吐瓶颈
- **执行步骤**：① Restate 集群部署（≥3 节点——官方集群指南）；② compose/
  部署脚本加集群拓扑；③ D3 演练改「单节点故障 → 集群接管」断言
- **验证**：kill 一节点 → 挂起审批 resolve 仍完成（awakeable 接管）

## 三、演练钩子

- **每周**：dr.yml schedule（周一 03:30 UTC——D1/D2/D3 三演练，RTO 实测复测）
- **月度全量**：dr.yml 的 monthly 输入（workflow_dispatch + 每月 1 日
  schedule）——三演练 + PITR 时长基线断言（> 5 分钟即 fail——RTO 回归哨兵）

## 四、SLO 监控告警（后置）

外部可观测系统（Prometheus/Grafana）接入时：run 延迟/SSE 首帧/RTO 的
指标导出（api 的 /metrics）——目标即本文档第一节；告警阈值 = 目标 × 2。
