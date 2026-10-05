# Executor 协议（worker ↔ executor）

> 权威定义：契约规范 §4。实现：`docker` driver（dev，受限容器）+ `e2b` driver（prod，
> Firecracker 微 VM，W4 在 Linux KVM 主机验证切流）。capability 字段（cpu/gpu/network/browser）
> 从第一天就在协议里，实现可后补。Go 定义见 `internal/execproto`。

## 端点

| 端点 | 语义 |
|---|---|
| `POST /sandboxes` | 创建 → `ready`；`{image, limits, ttl, capabilities, restore_from?}` |
| `POST /execute` | 流式日志 `{type:log\|exit}` + 结果 `{exit, output_ref}` |
| `GET/PUT /files/{sandbox_id}/{path}` | 文件快路径（不经过 shell） |
| `POST /sandboxes/{id}/freeze` / `unfreeze` | Tier 1（资源分级，落地方案 §12） |
| `POST /sandboxes/{id}/snapshot` → `{snapshot_ref}` | Tier 2 |
| `DELETE /sandboxes/{id}` | Tier 3（前提：`file_sync_state=synced`） |

## 请求/响应样例

```
POST /sandboxes
{ "image": "golang:1.23-bookworm", "limits": {"cpu": "1", "mem": "512Mi", "disk": "2Gi"},
  "ttl": "24h", "capabilities": {"network": true, "gpu": false} }
→ 201 { "sandbox_id": "sb_1", "status": "creating", "image": "golang:1.23-bookworm", "driver": "docker" }
```

```
POST /execute
{ "sandbox_id": "sb_1", "name": "bash", "input": "pytest",
  "ttl": "300s", "idempotency_key": "r_1:12:t_3" }
→ 200 text/event-stream: {"type":"log","payload":"..."} ... {"type":"exit","payload":{"exit":0,"output_ref":"s3://artifacts/..."}}
```

## 生命周期与幂等

- 生命周期：`creating → ready → frozen → snapshotted → destroyed`；
- `execute` 幂等键 = `(run_id, step, tool_id)`；TTL 空闲回收；
- **幂等缓存（防重试双执行，W2 前定型）**：executor 对 `idempotency_key` 的执行结果做缓存
  （PG `sandbox_execs` 表，TTL 24h，结果 >256KB 外置 MinIO）；
  exec 已执行但响应丢失 → 同键重发返回缓存结果，**绝不复跑命令**（git push 类非幂等命令的保命机制）；
- `sandboxes` 表（事实状态）：`file_sync_state: synced` 是 Tier3 拆除的前提；
  快照引用失效时降级为重构建。沙箱事实状态在 docker/PG，不随进程死。
