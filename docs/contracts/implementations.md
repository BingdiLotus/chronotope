# 参考实现清单

> 契约实现者索引。第三方实现按此清单自证兼容。

## 内置实现（随 repo 发布）

| 契约 | 实现 | 位置 |
|---|---|---|
| /runs（harness） | Python harness（fake/真实模型） | harness/app/ |
| executor 协议 | DockerDriver / E2BDriver（官方云+自托管） | internal/execproto/ |
| events | 事件 append-only + 哈希链（M1） | internal/store/events.go |

## 第三方接入步骤

1. 读契约：[runs-protocol.md](runs-protocol.md)、[executor-protocol.md](executor-protocol.md)、[seam-v1.md](seam-v1.md)
2. 实现协议面（harness 的 POST /runs 或 executor 的六端点）
3. 自证兼容：`go test ./test/contract/ -v`（帧级断言——用你的实现替换
   样例后跑通即兼容）
4. 注册：executor 走注册表（kind/capabilities）；harness 走 HARNESS_URL
5. 验证真实路径：w17-real-e2b.sh（真实 E2B 档）或 w1-loop（fake 闭环）

## 已知第三方生态（2026-10 调研）

| 生态 | 接入点 |
|---|---|
| dsh（deepseek-harness） | tool seam → execute 幂等键（Seam 的 compute/filesystem） |
| Pi（pi-durable） | harness 协议（/runs）——持久化 harness 层 |
| E2B | executor 协议（已内置官方云+自托管两档） |
