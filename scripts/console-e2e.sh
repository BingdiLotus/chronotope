#!/usr/bin/env bash
# 控制台 e2e 一键编排（profile 参数化——架构整洁 C5：三脚本合一）。
#   console  = 人控闭环三条用例（harness 审批脚本：bash class 2 强制审批）
#   tt       = 时空视图用例（checkpoint/fork/diff/rollback；harness plain）
#   runtree  = run 树拓扑图用例（harness spawn_subagent 脚本；child 动态注入）
# 前置：postgres/restate 运行中（docker compose up postgres restate）。
# 用法: bash scripts/console-e2e.sh [console|tt|runtree] [playwright 附加参数]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
export PATH="/opt/homebrew/bin:$PATH"
DB='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'
PROFILE="${1:-console}"

# 1. 停掉旧本地进程与 compose 应用容器（端口冲突防护）
pkill -f 'chronotope-(api|worker|executor)' 2>/dev/null || true
pkill -f 'uvicorn app.main:app' 2>/dev/null || true
docker compose --env-file .env -f deploy/docker-compose.yml stop api worker executor harness > /dev/null 2>&1 || true
sleep 1

# 2. 构建 + 各 profile 的 harness 脚本
go build -o bin/chronotope-api ./cmd/api
go build -o bin/chronotope-worker ./cmd/worker
go build -o bin/chronotope-executor ./cmd/executor

SCRIPT=""
EXTRA=""
case "$PROFILE" in
  console)
    SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"sleep 1; echo ok"}}},{"final":"危险操作已批准执行。"}]'
    ;;
  tt)
    SCRIPT=""
    ;;
  runtree)
    # child agent（harness 的 chronotope-subagent 分支确定性终答）
    nohup env DATABASE_URL="$DB" RESTATE_URL=http://localhost:8081 ./bin/chronotope-api -addr :8080 > /tmp/console-api.log 2>&1 &
    sleep 3
    CHILD_ID=$(curl -fsS -X POST http://localhost:8080/orgs/org-rt-child/agents -H 'content-type: application/json' \
      -d '{"name":"rt-child","config":{"model":"chronotope-subagent","instructions":"子任务助手。","tools":[],"version":1}}' \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
    SCRIPT='[{"tool_call":{"name":"spawn_subagent","arguments":{"agent":"'"$CHILD_ID"'","input":"计算 2+2"}}},{"final":"子任务已完成，父任务收尾。"}]'
    pkill -f 'chronotope-api' 2>/dev/null || true
    sleep 1
    ;;
  *)
    echo "未知 profile: $PROFILE（支持 console|tt|runtree）" >&2
    exit 2
    ;;
esac

# 3. 起栈
(cd harness && nohup env HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT="$SCRIPT" uv run uvicorn app.main:app --port 8000 > /tmp/console-harness.log 2>&1 &)
nohup env DATABASE_URL="$DB" ./bin/chronotope-executor -addr :9082 > /tmp/console-executor.log 2>&1 &
nohup env DATABASE_URL="$DB" HARNESS_URL=http://localhost:8000 EXECUTOR_URL=http://localhost:9082 ./bin/chronotope-worker -addr :9080 > /tmp/console-worker.log 2>&1 &
nohup env DATABASE_URL="$DB" RESTATE_URL=http://localhost:8081 ./bin/chronotope-api -addr :8080 > /tmp/console-api.log 2>&1 &
sleep 3
curl -fsS -X POST http://localhost:9070/deployments -H 'content-type: application/json' \
  -d '{"uri":"http://host.docker.internal:9080","version":"v1","use_http_11":true,"force":true}' > /dev/null
for u in 8000 9082 9080 8080; do curl -fsS "localhost:$u/healthz" > /dev/null; done

# 3.5 收尾恢复 compose 应用容器（脚本开头 stop 了它们——不留半停栈；
# 全量测试编排实证：不恢复会使后续套件环境损坏）
restore_compose_apps() {
  docker compose --env-file .env -f "$ROOT/deploy/docker-compose.yml" up -d api worker executor harness > /dev/null 2>&1 || true
  pkill -f 'chronotope-(api|worker|executor)' 2>/dev/null || true
  pkill -f 'uvicorn app.main:app' 2>/dev/null || true
}
trap restore_compose_apps EXIT

# 4. Playwright（profile → 测试文件）
cd web
case "$PROFILE" in
  console) timeout 300 pnpm exec playwright test console "${@:2}" ;;
  tt)      timeout 300 pnpm exec playwright test timetravel "${@:2}" ;;
  runtree) timeout 300 pnpm exec playwright test runtree "${@:2}" ;;
esac
