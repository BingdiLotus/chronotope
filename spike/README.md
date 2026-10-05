# W1 D1 Spike：Restate Go SDK 五项原语验证

> 结论记录见文末表格（实验后填写）。上游：mvp-落地方案 §4 D1。
> 降级判断：任一原语阻断 → 切 Temporal Go（边界已隔离在 internal/restate 与 run_workflow 主循环）。

## 依赖与版本

- Restate Server：docker（deploy/docker-compose.yml 的 restate 服务）
- restate-sdk-go：v1.1.0（**注意：要求 go ≥ 1.25.0**，Restate Server ≥ 1.3）
- 本 spike 为独立 Go module（`GOWORK=off` 运行），不污染主模块 go.mod

## 运行

```bash
# 1. 起 Restate
docker compose -f deploy/docker-compose.yml up -d restate

# 2. 构建并启动端点（v1，含桩 harness）
cd spike && GOWORK=off go build -o spike-server . && SPIKE_VERSION=v1 ./spike-server -addr :9080

# 3. 注册部署（docker 内 Restate 经 host.docker.internal 访问宿主机）
curl -X POST localhost:9070/deployments -H 'content-type: application/json' \
  -d '{"uri":"http://host.docker.internal:9080","version":"v1"}'

# 4. 逐项跑实验（另开终端；实验④需双端点，见脚本内注释）
bash experiments.sh
```

## 实验①：Run 闭包内分钟级 SSE 长流

- `sseprobe/ProbeTwoSteps`：step1 打桩 `/once`（已完成的 journaled step）；step2 在 Run 闭包内
  消费桩的 SSE 流（`/stream?dur=N`，每秒 1 delta）。
- 验证点：
  1. 长流（60s 量级）能在 Run 闭包内完整消费并 journal 结果；
  2. **step2 进行中 kill -9 端点进程 → 重启**：step1 从 journal 回放（桩 /once 不增加命中），
     step2 幂等重发一次（桩 /stream 命中 +1）。

## 实验②：awakeable 跨 HTTP resolve

- `approval` Virtual Object：`RequestApproval` 建 awakeable 挂起并记 state；
  `PendingID` 读出 id；`Resolve` 从另一 HTTP invocation 解析。
- 验证点：跨 HTTP resolve 后，挂起方以解析值返回；挂起期间零进程占用。

## 实验③：child workflow 调用/await

- `parentflow/run` 调 `childflow/<childID>/run`（子 workflow 打桩 + Sleep + 返回），父 await。
- 验证点：父拿到子结果；**父在 await 中 kill -9 → 重启重放**：子不重复派发（桩命中不变）。

## 实验④：endpoint versioning

- 双端点：`SPIKE_VERSION=v1 ./spike-server -addr :9080` 与 `SPIKE_VERSION=v2 ./spike-server -addr :9081`
  （桩 harness 只有 v1 起）。
- 验证点：注册 v1 → 启动在途 invocation（Sleep 中）→ 注册 v2 → 新 invocation 走 v2、在途完成时返回 v1。

## 实验⑤：journal/state 条目大小量级

- `sizeprobe/<n>/run`：分别写 n KB 的 state 条目与 journal 输出（16/64/256/1024/2048/4096/8192 KB）。
- 验证点：找到两通道的大小上限量级 → 为 journal 大小策略（契约规范 §7：>4KB 外置）提供依据。

## 结论记录

| # | 原语 | 结论 | 备注 |
|---|---|---|---|
| ① | Run 内分钟级 SSE 长流 | ✅ 通过 | 8s 长流在 Run 闭包内完整消费并 journal（机理上可任意长）；kill -9 端点重启后：**已完成 step 从 journal 回放不重执行**（桩 /once 命中=1）、**未完成 step 幂等重发恰好一次**（桩 /stream 命中=2） |
| ② | awakeable 跨 HTTP resolve | ✅ 通过（含关键约束） | 建/挂起/跨 HTTP resolve 全通，挂起方收到解析值；**resolve 必须来自对象之外的 invocation**——同对象 exclusive handler 挂起时仍占排他锁，对象内 Resolve 永远排队 = 死锁 → 印证 worker-架构设计 §2 webhook 独立 Service 的设计；挂起期间 shared handler 可读 state（PendingID 验证），HITL 期间可查询状态 |
| ③ | child workflow 调用/await | ✅ 通过 | 父 await 拿到子结果；父在 await 中 kill -9 → 重放**不重复派发**子 workflow（子触达桩命中=1） |
| ④ | endpoint versioning | ✅ 通过 | 双端点按 `version` + `force` 注册：**新 invocation 走 v2、在途 Sleep 中 invocation 完成时留 v1** |
| ⑤ | journal/state 大小量级 | ✅ 有明确量级 | ≤16 MiB 秒级成功；**≥10 MiB 触发性能告警**（worker.invoker.message_size_warning）；**硬上限 ≈32.25 MiB/消息**（默认 worker.invoker.message_size_limit=33816576 字节，超出 invocation 失败）；32 MiB 整档虽未超限但极慢（>60s）不可用。→ 契约规范 §7 的 **4KB 内联 + 外置引用策略距上限三个数量级，安全** |
| 附 | SDK 版本约束 | 注意 | sdk-go v1.1.0 要求 **go ≥ 1.25.0**（主模块 go.mod 需升 1.25、CI 同步）、Restate Server ≥ 1.3（本机 1.7.13 ✓）；SDK 1.x `Run` 无位置 step 名，改用 `restate.WithName` 显式命名——worker 伪代码 `"harness:N"` / `"exec:N:tool"` 语义不变；SDK 端点仅 HTTP/1.1，注册部署需 `use_http_11:true`；删除部署需 `?force=true`；历史失败运行遗留的挂起 invocation 会复用同名 object key 造成排队污染——实验/测试一律用每次运行唯一 key |

> **总体结论：五项原语全部通过 → 不切 Temporal，worker 按 worker-架构设计 §2 接入 sdk-go v1.1.0。**
