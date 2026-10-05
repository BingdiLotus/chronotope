#!/usr/bin/env bash
# W3 验收 e2e ③：崩溃恢复 chaos——run 执行中 kill -9 worker → 重启自动续跑，
# 日志证明「不重调 harness、不重跑沙箱」（已完成 step 走 journal 缓存；
# 在途 exec 由 executor 幂等缓存兜底，绝不复跑命令）。
#
# 自包含：本脚本接管 worker/harness/executor 生命周期（api 保持运行）。
# 场景：3 步 bash（各 sleep 2s）→ 第 1 步完成、第 2 步执行中 kill -9 worker。
# 用法: bash test/chaos/kill9-worker.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
DB='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'
BIN="$(cd "$(dirname "$0")/../.." && pwd)/bin"
HARNESS_DIR="$(cd "$(dirname "$0")/../.." && pwd)/harness"
RUN_ID="chaos-$(date +%s)"
PASS=0; FAIL=0

pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W3 崩溃恢复 chaos（API=${API}，RUN_ID=${RUN_ID}）=="

# --- 0. 接管服务（干净日志便于计数）---
pkill -f 'chronotope-worker' 2>/dev/null || true
pkill -f 'uvicorn app.main:app' 2>/dev/null || true
pkill -f 'chronotope-executor' 2>/dev/null || true
sleep 1

SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo one"}}},{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo two"}}},{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo three"}}},{"final":"崩溃恢复完成"}]'
(cd "$HARNESS_DIR" && nohup env HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT="$SCRIPT" uv run uvicorn app.main:app --port 8000 > /tmp/chaos-harness.log 2>&1 &)
nohup env DATABASE_URL="$DB" "$BIN/chronotope-executor" -addr :9082 > /tmp/chaos-executor.log 2>&1 &
start_worker() { nohup env DATABASE_URL="$DB" HARNESS_URL=http://localhost:8000 EXECUTOR_URL=http://localhost:9082 "$BIN/chronotope-worker" -addr :9080 > /tmp/chaos-worker.log 2>&1 & }
start_worker
sleep 3
for u in 8000 9082 9080; do curl -fsS "localhost:$u/healthz" > /dev/null; done

HARNESS_BASELINE=$(grep -c 'POST /runs' /tmp/chaos-harness.log || true)
EXEC_BASELINE=$(grep -c 'INFO execute' /tmp/chaos-executor.log || true)

# --- 1. agent（bash 工具）+ session + SSE ---
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"chaos-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是崩溃恢复演示助手。","tools":["bash"],"environment":{"sandbox":{"image":"python:3.11-slim","limits":{"cpu":"1","mem":"256m"},"ttl":"1h"}},"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/chaos-sse.out 2>&1 &
SSE_PID=$!
sleep 1

# --- 2. 提交任务（后台）→ 第 1 步完成、第 2 步执行中 → kill -9 worker ---
curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"跑三步命令证明崩溃恢复"}' > /tmp/chaos-run.out &
RUN_PID=$!

# 等第 1 个 sandbox.exec（第 1 步已完成并 journal）
for i in $(seq 1 60); do
  [ "$(grep -c '"type":"sandbox.exec"' /tmp/chaos-sse.out 2>/dev/null || true)" -ge 1 ] && break
  sleep 1
done
sleep 1  # 此刻第 2 步 exec 执行中
WORKER_PID=$(pgrep -f 'chronotope-worker -addr' | head -1)
kill -9 "$WORKER_PID"
echo "  [注入] kill -9 worker (pid=$WORKER_PID)，第 2 步 exec 执行中"

# --- 3. 重启 worker（同地址，部署注册不失效）→ 自动续跑 ---
sleep 1
start_worker
sleep 2

wait "$RUN_PID" || true
RUN=$(cat /tmp/chaos-run.out)
assert "崩溃后自动续跑完成（final=崩溃恢复完成，steps=4）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and "崩溃恢复完成" in d["final"] and d["steps"]==4' <<< "$RUN"

sleep 1
kill "$SSE_PID" 2>/dev/null || true

# --- 4. 事件完整性：3×tool.call + 3×sandbox.exec（dedupe 幂等）+ run.completed ---
assert "sandbox.exec ×3（重放不重复发射）" \
  test "$(grep -c '"type":"sandbox.exec"' /tmp/chaos-sse.out || true)" -eq 3
assert "tool.call ×3" test "$(grep -c '"type":"tool.call"' /tmp/chaos-sse.out || true)" -eq 3
assert "run.completed" grep -q '"type":"run.completed"' /tmp/chaos-sse.out

# --- 5. 不重调/不重跑证明 ---
HARNESS_DELTA=$(( $(grep -c 'POST /runs' /tmp/chaos-harness.log || true) - HARNESS_BASELINE ))
EXEC_DELTA=$(( $(grep -c 'INFO execute' /tmp/chaos-executor.log || true) - EXEC_BASELINE ))
echo "  harness /runs 增量: ${HARNESS_DELTA}（期望 4：4 个 step 各一次，零重调）"
echo "  executor 执行增量: ${EXEC_DELTA}（期望 4：3 步各一次 + 在途 step 幂等重发恰好一次）"
assert "不重调 harness（/runs 恰好 4 次，已完成 step 零重调）" test "$HARNESS_DELTA" -eq 4
assert "沙箱执行语义正确（3 步各一次 + 在途 step 重发恰好一次 = 4）" test "$EXEC_DELTA" -eq 4

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
