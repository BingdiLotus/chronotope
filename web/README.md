# chronotope-web（Next.js 控制台，W4）

> 唯一 TS 面（mvp-技术选型 §8.1）。W4 范围：会话列表 / 时间轴回放 / 用量三轴。

## 运行

```bash
pnpm install
API_URL=http://localhost:8080 pnpm dev   # http://localhost:3000（rewrites /api/* → API_URL）
```

容器化：`docker compose -f deploy/docker-compose.yml --profile web up --build web`。

## 已实现（W4 + P2-2 人控闭环）

- `/`：会话列表（默认 org-demo，可改；GET /api/orgs/:id/sessions）
- `/sessions/[id]`：时间轴回放（SSE `after=seq` 断线续读，容忍 gap）+ 三轴计量表
  + **人控操作区**（class 2 审批批准/拒绝按钮 → webhook；欠费冻结解冻按钮 →
  actions unfreeze）+ **分层记忆面板**（主题摘要 + 长期记忆条目）
- 跨源直连：页面经 `NEXT_PUBLIC_API_URL`（默认 http://localhost:8080）直连 API
  （SSE 不能经 next rewrite 代理——会被缓冲）；API 侧 CORS 白名单
  `API_CORS_ORIGINS`（默认 http://localhost:3000）

## e2e（Playwright）

```bash
bash scripts/console-e2e.sh   # 一键：起本地栈（审批脚本模式）+ 跑 3 条用例
# 3 用例：会话页渲染（时间轴/记忆/人控区）、批准→run 完成、拒绝→tool_denied
```

## 后置

- 用量聚合图表、org 预算充值 UI（现走 API：PUT /orgs/:id/budget）

