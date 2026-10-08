# BYOC 档部署（期 4 §C）

BYOC（Bring Your Own Cloud）：E2B 沙箱跑在**租户自有云账户**——协议与
E2B 官方云完全同源（envd ConnectRPC + 平台 REST），平台侧零协议改动；
差异只在**凭证与模板**指向租户的 E2B 实例。

## 架构

```
agent.config.environment.sandbox.driver: "byoc"
        │ worker 的 executor 池按 kind=byoc 过滤候选（期 4 §C 路由）
        ▼
executor 进程（EXECUTOR_DRIVER=byoc → E2BDriver + kind=byoc 注册）
        │ E2B_API_URL/E2B_API_KEY → 租户 E2B 实例
        ▼
租户云沙箱（BYOC 模板镜像）
```

## 部署步骤

### 1. 构建 BYOC 模板镜像（租户侧）

```bash
# 以租户的基础镜像构建（E2B 模板要求 envd ≥ v0.5.0）
bash deploy/images/build-byoc.sh <registry>/<image>:<tag>
```

### 2. 注册模板到租户 E2B 控制台

在租户 E2B 控制台创建模板（指向步骤 1 的镜像）——记下模板 ID。

### 3. 平台侧配置（.env）

```bash
EXECUTOR_DRIVER=byoc
E2B_API_URL=https://api.租户域            # 租户 E2B 实例（自托管/专属）
E2B_API_KEY=租户实例凭证
E2B_TEMPLATE_MAP={"*":"<模板 ID>"}        # 镜像名 → 租户模板映射
```

### 4. 起 executor + 验证

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate executor
bash test/e2e/e2b-smoke.sh                # 全链路（命令/文件）
```

### 5. 租户 agent 走 byoc 档

```json
{"name":"byoc-agent","config":{"model":"...","instructions":"...","tools":["bash"],
 "environment":{"sandbox":{"driver":"byoc","image":"<registry>/<image>:<tag>"}},"version":1}}
```

## 路由语义

- `driver` 空 → 池默认（全候选——现有部署无感）
- `driver: "byoc"` → 只选注册表 `kind=byoc` 的 executor（租户隔离）
- 候选无匹配 → 单点兜底（EXECUTOR_URL——旧部署兼容）

## 验证矩阵

| 层 | 验证 |
|---|---|
| 单测 | 档过滤（byoc 请求只选 byoc 候选——TestExecutorPoolDriverFilter） |
| fake e2e | docker 档全量 CI 不回归（demo 20 阶段） |
| 真机 | e2b-smoke 参数化——BYOC 凭证就绪即跑（后置） |
