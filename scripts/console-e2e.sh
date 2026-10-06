#!/usr/bin/env bash
# 控制台 e2e 一键编排（P2-2 人控闭环）：
# 本地起 api/worker/harness（harness 审批脚本模式）→ 安装 web 依赖/浏览器（首次）
# → Playwright 三条用例（渲染 / 批准 / 拒绝）。
# 前置：postgres/restate 运行中（make dev-up）。
# 用法: bash scripts/console-e2e.sh
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="/opt/homebrew/bin:$PATH"
DB='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'

# 1. 停掉旧本地进程与 compose 应用容器（端口冲突防护）
pkill -f 'chronotope-(api|worker|executor)' 2>/dev/null || true
pkill -f 'uvicorn app.main:app' 2>/dev/null || true
docker compose --env-file .env -f deploy/docker-compose.yml stop api worker executor harness > /dev/null 2>&1 || true
sleep 1

# 2. 起本地栈：harness 审批脚本（bash class 2 强制审批场景）
go build -o bin/chronotope-api ./cmd/api
go build -o bin/chronotope-worker ./cmd/worker
go build -o bin/chronotope-executor ./cmd/executor
SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"sleep 1; echo ok"}}},{"final":"危险操作已批准执行。"}]'
(cd harness && nohup env HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT="$SCRIPT" uv run uvicorn app.main:app --port 8000 > /tmp/console-harness.log 2>&1 &)
nohup env DATABASE_URL="$DB" ./bin/chronotope-executor -addr :9082 > /tmp/console-executor.log 2>&1 &
nohup env DATABASE_URL="$DB" HARNESS_URL=http://localhost:8000 EXECUTOR_URL=http://localhost:9082 ./bin/chronotope-worker -addr :9080 > /tmp/console-worker.log 2>&1 &
nohup env DATABASE_URL="$DB" RESTATE_URL=http://localhost:8081 ./bin/chronotope-api -addr :8080 > /tmp/console-api.log 2>&1 &
sleep 3
curl -fsS -X POST http://localhost:9070/deployments -H 'content-type: application/json' \
  -d '{"uri":"http://host.docker.internal:9080","version":"v1","use_http_11":true,"force":true}' > /dev/null
for u in 8000 9082 9080 8080; do curl -fsS "localhost:$u/healthz" > /dev/null; done

# 3. Playwright（首次自动下载 chromium）
cd web
pnpm exec playwright test "$@"
