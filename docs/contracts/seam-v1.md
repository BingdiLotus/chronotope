# Seam 接缝 v1（2026-10-09）

> 版本：v1.0 ｜ 只增不改 ｜ 本文件是**可替换接缝的一等公民声明**——第三方
> （dsh/Pi/E2B 等）零侵入接入的入口。

## 什么是 Seam

Seam 是**可替换能力接缝**：executor 协议的 `capabilities` 字段声明的能力，
第三方实现同一个 Seam 即可零侵入替换系统能力（dsh 的 Seam 概念同思路——
此处把它作为契约的一等公民命名）。

## Seam 清单（v1）

| Seam | 协议字段 | 说明 | 内置实现 |
|---|---|---|---|
| `compute` | capabilities.compute | 代码执行（bash/python/CLI） | docker / E2B |
| `filesystem` | capabilities.filesystem | 文件读写（PUT/GET /files） | docker cp / E2B files |
| `network` | capabilities.network | 网络出口（可选隔离） | 当前 true |
| `snapshot` | capabilities.snapshot | 快照/恢复（Tier2） | docker commit+tar / E2B snapshot |
| `freeze` | capabilities.freeze | 暂停/恢复（Tier1） | docker pause / E2B pause |
| `harness` | /runs 协议 | 模型推理（worker ↔ harness 唯一协议） | 内置 Python harness / 第三方 |

## 第三方接入（零平台改动）

1. 实现 executor 协议（POST /sandboxes、/execute、GET/PUT /files、/freeze、
   /snapshot、DELETE）——`capabilities` 声明支持的 Seam
2. 或实现 harness 协议（POST /runs 的请求/SSE 帧——runs-protocol.md）——
   `/runs` 契约设计本就支持 0 平台改动换 harness
3. 用一致性套件自证：`go test ./test/contract/`（协议帧级断言——与内置
   实现同源样例）
4. 注册进 executor 注册表（EXECUTOR_ENDPOINT + kind + capabilities）——
   worker 按 driver 档路由

## 幂等与对账（Seam 之上的不变式）

无论哪个 Seam 实现，以下不变式不变：幂等键贯穿（execute 的
`idempotency_key`、harness 的 `(run_id, step)`）、effect ledger 的
prepared/done、审计链。**Seam 换的是能力，不是语义**——第三方实现的
副作用同样可对账。
