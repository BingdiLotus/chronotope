#!/usr/bin/env bash
# 时空视图 e2e 一键编排（期 2 §C）：
# 本地起 api/worker/harness（harness fake plain 模式）→ Playwright 时空视图用例
# （checkpoint / fork 血缘 / diff / rollback 审计）。
# 前置：postgres/restate 运行中（docker compose up postgres restate）。
# 用法: bash scripts/console-tt-e2e.sh
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="/opt/homebrew/bin:$PATH"
DB='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'

# 1. 停掉旧本地进程与 compose 应用容器（端口冲突防护）
pkill -f 'chronotope-(api|worker|executor)' 2>/dev/null || true
pkill -f 'uvicorn app.main:app' 2>/dev/null || true
docker compose --env-file .env -f deploy/docker-compose.yml stop api worker executor harness > /dev/null 2>&1 || true
sleep 1

# 2. 起本地栈：harness fake plain（时间旅行 e2e 不需要工具）
go build -o bin/chronotope-api ./cmd/api
go build -o bin/chronotope-worker ./cmd/worker
go build -o bin/chronotope-executor ./cmd/executor
(cd harness && nohup env HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= uv run uvicorn app.main:app --port 8000 > /tmp/console-tt-harness.log 2>&1 &)
nohup env DATABASE_URL="$DB" ./bin/chronotope-executor -addr :9082 > /tmp/console-tt-executor.log 2>&1 &
nohup env DATABASE_URL="$DB" HARNESS_URL=http://localhost:8000 EXECUTOR_URL=http://localhost:9082 ./bin/chronotope-worker -addr :9080 > /tmp/console-tt-worker.log 2>&1 &
nohup env DATABASE_URL="$DB" RESTATE_URL=http://localhost:8081 ./bin/chronotope-api -addr :8080 > /tmp/console-tt-api.log 2>&1 &
sleep 3
curl -fsS -X POST http://localhost:9070/deployments -H 'content-type: application/json' \
  -d '{"uri":"http://host.docker.internal:9080","version":"v1","use_http_11":true,"force":true}' > /dev/null
for u in 8000 9082 9080 8080; do curl -fsS "localhost:$u/healthz" > /dev/null; done

# 3. Playwright（仅时空视图用例）
cd web
pnpm exec playwright test timetravel "$@"
