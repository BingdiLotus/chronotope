# 调研：E2B 本地部署与协议兼容替代（2026-10）

> 目标：为「本地 E2B 服务部署 / 同类协议兼容替代」给出可行路径与取舍。
> 结论先行：**本地开发用现有 docker 档（功能等价且自有全链路）；生产/合规的
> 协议兼容选项 = 阿里云 Agent Sandbox（E2B 数据面协议兼容，E2B_API_URL 切换
> 即试）；官方自托管（infra 仓库）是重型路线，MVP 阶段不推荐。**

## 一、E2B 官方自托管（infra 仓库）

- **开源边界**：`github.com/e2b-dev/infra` 公开可克隆——API/orchestrator/
  envd/client-proxy/模板构建全在；但**托管模型留在 E2B 侧的是运维**（暖池、
  快照存储、网络 fabric、滥用与计费）。
- **生产路径**：GCP + Nomad + Consul + Terraform——五 Go 服务 + Postgres/
  ClickHouse/Redis + Loki/Tempo/Mimir/Grafana/Vector；**单宿主 dev 路径存在**
  （Compose + 裸进程）。
- **运维真相**（bex.co 实证文章）：Firecracker 是容易的 5%，控制面 95%——
  调度/快照存储/网络 fabric/模板流水线/envd 三向兼容矩阵/可观测/治理八块。
  低于数千 sandbox-hours/月的突发负载，托管更划算。
- **判定**：真自托管 = 采纳一套分布式系统并配人运维——**不推荐 MVP 阶段**。

## 二、e2b-local（superduck-ai/e2b-local）——本地兼容网关

- **形态**：跑在本机的 E2B 兼容网关——SDK 调用方式不变（`E2B_API_URL` 指
  本地），sandbox 落到本机 Docker 容器 / OrbStack Linux VM。
- **架构**：控制面 gateway（Gin + E2B OpenAPI DTO 生成）+ **数据面 envd 从
  E2B 源码构建**（协议原生、非重写）——template/volume 翻译为 Docker 引擎
  API / OrbStack UDS JSON-RPC。
- **未实现**：snapshot（快照语义复杂——作者明说）；metrics/logs/network
  policy/配额等平台能力。
- **定位**：template 开发调试工具（缩短反馈链路），非生产控制面。
- **对我们的价值**：我们的 `HTTPE2BAPI` 自托管协议（`E2B_API_URL` 指向的
  网关）正是接它的口——但**本地开发我们的 docker 档已覆盖**（协议不同但
  功能等价、全链路自有）。仅在需要「第三方用 E2B SDK 直连我们」的兼容场景
  才引入。

## 三、阿里云 Agent Sandbox 极速版——E2B 协议兼容的国内云

- **兼容性**：**兼容 E2B 数据面协议**（阿里云函数计算团队维护 Java/Go SDK：
  `aliyun-fc/e2b-java-sdk`、`aliyun-fc/e2b-go-sdk`）——配置与 E2B 同形状：
  ```bash
  E2B_API_KEY / E2B_API_URL（https://api.<region>.sandbox.aliyuncs.com）/ E2B_DOMAIN
  ```
- **地域**：cn-beijing/shanghai/hangzhou/shenzhen/hongkong + ap-southeast-1 +
  us-east-1/us-west-1（8 个）。
- **能力边界**：官方文档有「E2B SDK 兼容 API 清单」（接口与默认值不完全
  等同——接入前核对）。
- **对我们的价值**：**E2B 档的国内合规后端**——我们的 `HTTPE2BAPI` 自托管
  协议分支预期直接兼容（E2B_API_URL 切换 + 凭证即试）；验证方法：配三个
  env → 跑 `test/e2e/e2b-smoke.sh`（真机断言）。

## 四、同类产品（协议不兼容——接入需新 driver）

| 产品 | 隔离 | 冷启动 | 协议 | 判定 |
|---|---|---|---|---|
| Daytona | Firecracker workspaces | <90ms 暖 | 自有（IDE 形态） | 不是沙箱 API；license 2026 中收紧 |
| Modal / serverless 容器 | gVisor + 快照恢复 | ~100ms | 自有 | GPU/批量场景强，非会话内核隔离 |
| K8s SIG agent-sandbox | 容器 | — | K8s 对象 | 生命周期 K8s 化——不是 E2B 协议 |
| 单宿主 Firecracker 包装 | microVM | 125ms | 自有 | 单机私有场景，无舰队机器 |

## 五、结论与建议

1. **本地开发**：维持 docker 档（功能等价、自有全链路、零新依赖）。
2. **生产/合规的协议兼容选项**：**阿里云 Agent Sandbox**——E2B 数据面协议
   兼容 + 国内地域；验证成本 = 三个 env + smoke 脚本（后置凭证）。
3. **官方自托管**：重型（八块控制面 + 五服务矩阵）——量级到数千
   sandbox-hours/月持续高并发前不启动。
4. **e2b-local**：仅在「E2B SDK 直连本地」的第三方兼容场景引入。

## 附：行动清单（可选）

- [ ] 阿里云 Agent Sandbox 凭证就绪 → 配 E2B_API_URL/KEY/DOMAIN → 跑 smoke
      （若 HTTPE2BAPI 自托管分支端点形状不符，按兼容 API 清单小修）
- [ ] 国内合规需求确认 → E2B 档切换阿里云后端（零代码或小修）
