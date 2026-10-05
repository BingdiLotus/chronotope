#!/usr/bin/env bash
# chaos 套件共享库（kill -9 矩阵的公共设施）。
# 约定：脚本接管本地 api/worker/executor/harness 进程生命周期（compose 只提供
# postgres/restate）；compose 应用容器会被 stop，避免与本地二进制端口冲突。
# 注意：先于 compose 应用容器运行 chaos（demo.sh 之后需先 stop 容器）。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DB="${DB:-postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable}"
BIN="$ROOT/bin"
HARNESS_DIR="$ROOT/harness"
# compose 一律绝对路径（脚本会 cd 到 test/chaos，相对路径会静默失效——实证）
ENVFILE_ARGS=()
[ -f "$ROOT/.env" ] && ENVFILE_ARGS=(--env-file "$ROOT/.env")
COMPOSE=(docker compose "${ENVFILE_ARGS[@]}" -f "$ROOT/deploy/docker-compose.yml")
ADMIN=http://localhost:9070

start_worker() {
  nohup env DATABASE_URL="$DB" HARNESS_URL=http://localhost:8000 EXECUTOR_URL=http://localhost:9082 \
    "$BIN/chronotope-worker" -addr :9080 >> /tmp/chaos-worker.log 2>&1 &
}

start_executor() {
  nohup env DATABASE_URL="$DB" \
    "$BIN/chronotope-executor" -addr :9082 >> /tmp/chaos-executor.log 2>&1 &
}

start_harness() { # $1 = fake 脚本 JSON（可为空）
  (cd "$HARNESS_DIR" && nohup env HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT="${1:-}" \
    uv run uvicorn app.main:app --port 8000 >> /tmp/chaos-harness.log 2>&1 &)
}

start_api() {
  nohup env DATABASE_URL="$DB" RESTATE_URL=http://localhost:8081 \
    "$BIN/chronotope-api" -addr :8080 >> /tmp/chaos-api.log 2>&1 &
}

stop_local_services() {
  pkill -f 'chronotope-(api|worker|executor)' 2>/dev/null || true
  pkill -f 'uvicorn app.main:app' 2>/dev/null || true
  sleep 1
  # 日志清空（start_* 全部追加写，跨重启累计计数）
  : > /tmp/chaos-harness.log
  : > /tmp/chaos-executor.log
  : > /tmp/chaos-worker.log
  : > /tmp/chaos-api.log
}

stop_compose_apps() { # 防 compose 应用容器与本地二进制端口冲突（postgres/restate 保留）
  "${COMPOSE[@]}" stop api worker executor harness > /dev/null 2>&1 || true
}

ensure_api() { # 本地 api 未运行则启动（chaos 全程本地进程）
  if ! curl -fsS http://localhost:8080/healthz > /dev/null 2>&1; then
    start_api
    sleep 2
  fi
}

register_worker() {
  curl -fsS -X POST "$ADMIN/deployments" -H 'content-type: application/json' \
    -d '{"uri":"http://host.docker.internal:9080","version":"v1","use_http_11":true,"force":true}' > /dev/null
}

wait_for_service() { # $1=url $2=尝试次数
  local url="$1" tries="${2:-30}"
  for _ in $(seq 1 "$tries"); do
    curl -fsS "$url" > /dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

# seed_agent <api> <tools_json> → stdout "AID SID"
seed_agent() {
  local api="$1" tools="$2" agent aid sid
  agent=$(curl -fsS -X POST "$api/orgs/org-chaos/agents" -H 'content-type: application/json' -d "{
    \"name\":\"chaos-agent\",
    \"config\":{
      \"model\":\"m\",\"instructions\":\"i\",\"tools\":$tools,
      \"environment\":{\"sandbox\":{\"image\":\"python:3.11-slim\",\"limits\":{\"cpu\":\"1\",\"mem\":\"256m\"},\"ttl\":\"1h\"}},
      \"version\":1}}")
  aid=$(echo "$agent" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
  sid=$(curl -fsS -X POST "$api/agents/$aid/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
  echo "$aid $sid"
}

# wait_for_count <file> <pattern> <期望数> <超时秒>
wait_for_count() {
  local file="$1" pattern="$2" want="$3" timeout="${4:-60}"
  for _ in $(seq 1 "$timeout"); do
    [ "$(grep -c "$pattern" "$file" 2>/dev/null || true)" -ge "$want" ] && return 0
    sleep 1
  done
  return 1
}

count_of() { # count_of <file> <pattern>
  grep -c "$2" "$1" 2>/dev/null || true
}

# 通用断言（bash 3.2 安全：变量全部 ${} 包裹，避免全角字符吞变量名）
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}
finish() { # 汇总并退出
  echo "== 结果: ${PASS} 通过, ${FAIL} 失败 =="
  [ "$FAIL" -eq 0 ]
}
