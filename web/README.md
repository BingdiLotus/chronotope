# chronotope-web（Next.js 控制台，W4）

> 唯一 TS 面（mvp-技术选型 §8.1）。W4 范围：会话列表 / 时间轴回放 / 用量三轴。

## 运行

```bash
pnpm install
API_URL=http://localhost:8080 pnpm dev   # http://localhost:3000（rewrites /api/* → API_URL）
```

容器化：`docker compose -f deploy/docker-compose.yml --profile web up --build web`。

## 已实现（W4）

- `/`：会话列表（默认 org-demo，可改；GET /api/orgs/:id/sessions）
- `/sessions/[id]`：时间轴回放（SSE `after=seq` 断线续读，容忍 gap）+ 三轴计量表
  （GET /api/sessions/:id/usage，1min 桶）

## 后置

- 用量聚合图表、会话创建/审批操作 UI（审批走 API：POST /webhooks/approval/{runID}）

