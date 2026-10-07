#!/usr/bin/env bash
# run 树拓扑图 e2e 一键编排（期 2 §C 后置 #3）：
# 本地起 api/worker/harness（harness 带 spawn_subagent 脚本——child id 动态注入）
# → Playwright runtree 用例（派生边 + 子任务终态）。
# 前置：postgres/restate 运行中。
# 用法: bash scripts/console-rt-e2e.sh
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="/opt/homebrew/bin:$PATH"
DB='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'

# 1. 停旧进程与 compose 应用容器
pkill -f 'chronotope-(api|worker|executor)' 2>/dev/null || true
pkill -f 'uvicorn app.main:app' 2>/dev/null || true
docker compose --env-file .env -f deploy/docker-compose.yml stop api worker executor harness > /dev/null 2>&1 || true
sleep 1

# 2. child agent（harness 的 chronotope-subagent 分支确定性终答）
CHILD_ID=$(curl -fsS -X POST http://localhost:8080/orgs/org-rt-child/agents -H 'content-type: application/json' \
  -d '{"name":"rt-child","config":{"model":"chronotope-subagent","instructions":"子任务助手。","tools":[],"version":1}}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])' 2>/dev/null || echo "")

# 3. 起栈（api 先起以建 child agent；若上一步失败则起后重试）
go build -o bin/chronotope-api ./cmd/api
go build -o bin/chronotope-worker ./cmd/worker
go build -o bin/chronotope-executor ./cmd/executor
(cd harness && nohup env HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"spawn_subagent","arguments":{"agent":"'"$CHILD_ID"'","input":"计算 2+2"}}},{"final":"子任务已完成，父任务收尾。"}]' uv run uvicorn app.main:app --port 8000 > /tmp/console-rt-harness.log 2>&1 &)
nohup env DATABASE_URL="$DB" ./bin/chronotope-executor -addr :9082 > /tmp/console-rt-executor.log 2>&1 &
nohup env DATABASE_URL="$DB" HARNESS_URL=http://localhost:8000 EXECUTOR_URL=http://localhost:9082 ./bin/chronotope-worker -addr :9080 > /tmp/console-rt-worker.log 2>&1 &
nohup env DATABASE_URL="$DB" RESTATE_URL=http://localhost:8081 ./bin/chronotope-api -addr :8080 > /tmp/console-rt-api.log 2>&1 &
sleep 3
curl -fsS -X POST http://localhost:9070/deployments -H 'content-type: application/json' \
  -d '{"uri":"http://host.docker.internal:9080","version":"v1","use_http_11":true,"force":true}' > /dev/null
for u in 8000 9082 9080 8080; do curl -fsS "localhost:$u/healthz" > /dev/null; done

# 4. Playwright（runtree 用例）
cd web
pnpm exec playwright test runtree "$@"
