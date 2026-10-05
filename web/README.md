# chronotope-web（Next.js 控制台，W4）

> 唯一 TS 面（mvp-技术选型 §8.1）。W4 范围：会话列表 / 时间轴回放 / 用量三轴。

## 运行

```bash
pnpm install
pnpm dev          # http://localhost:3000
```

容器化：`docker compose -f deploy/docker-compose.yml --profile web up --build web`。

## W4 TODO

- 会话列表页（api：GET /sessions 列表端点随 W4 补）
- 时间轴回放页（`GET /sessions/:id/events?after=seq` SSE 断线续读，容忍 seq gap）
- 用量页（usage 表 1min 桶聚合三轴）
