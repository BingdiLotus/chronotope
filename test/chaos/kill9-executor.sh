#!/usr/bin/env bash
# W3 崩溃恢复 chaos：kill -9 executor（第 2 步 exec 执行中）→ 重启后
# worker 重发 execute；沙箱按 sandbox_id 存活（容器不随进程死），重发恰好一次。
# 用法: bash test/chaos/kill9-executor.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

API="${1:-http://localhost:8080}"
RUN_ID="chaos-e-$(date +%s)"
SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo one"}}},{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo two"}}},{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo three"}}},{"final":"崩溃恢复完成"}]'

echo "== 崩溃恢复 chaos：kill -9 executor（第 2 步 exec 执行中）=="

stop_local_services; stop_compose_apps; ensure_api
start_harness "$SCRIPT"; start_executor; start_worker
wait_for_service http://localhost:8000/healthz; wait_for_service http://localhost:9082/healthz; wait_for_service http://localhost:9080/healthz
register_worker

HARNESS_BASELINE=$(count_of /tmp/chaos-harness.log 'runs start')
EXEC_BASELINE=$(count_of /tmp/chaos-executor.log 'INFO execute')

read -r _ SID <<< "$(seed_agent "$API" '["bash"]')"
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/chaos-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"executor 崩溃恢复"}' > /tmp/chaos-run.out &
RUN_PID=$!

wait_for_count /tmp/chaos-sse.out 'sandbox.exec' 1 60
sleep 1 # 第 2 步 exec 执行中
EXEC_PID=$(pgrep -f 'chronotope-executor -addr' | head -1)
kill -9 "$EXEC_PID"
echo "  [注入] kill -9 executor (pid=${EXEC_PID})"

sleep 1
start_executor
wait_for_service http://localhost:9082/healthz
wait "$RUN_PID" || true
RUN=$(cat /tmp/chaos-run.out)
assert "崩溃后自动续跑完成（steps=4）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and "崩溃恢复完成" in d["final"] and d["steps"]==4' <<< "$RUN"

sleep 1
kill "$SSE_PID" 2>/dev/null || true
assert "sandbox.exec ×3" test "$(count_of /tmp/chaos-sse.out 'sandbox.exec')" -eq 3

HARNESS_DELTA=$(( $(count_of /tmp/chaos-harness.log 'runs start') - HARNESS_BASELINE ))
EXEC_DELTA=$(( $(count_of /tmp/chaos-executor.log 'INFO execute') - EXEC_BASELINE ))
SANDBOX_IDS=$(grep -o 'sandbox_id=sb_[a-f0-9]*' /tmp/chaos-executor.log | sort -u | wc -l | tr -d ' ')
echo "  harness /runs 增量: ${HARNESS_DELTA}（期望 4：已完成 step 零重调）"
echo "  executor 执行增量: ${EXEC_DELTA}（期望 4：3 步 + 在途重发恰好一次）"
echo "  executor 日志中唯一 sandbox_id 数: ${SANDBOX_IDS}（期望 1：沙箱按 id 存活，不重建）"
assert "不重调 harness（/runs 恰好 4 次）" test "$HARNESS_DELTA" -eq 4
assert "execute 恰好 4 次（在途重发一次）" test "$EXEC_DELTA" -eq 4
assert "沙箱按 sandbox_id 存活（唯一 id=1）" test "$SANDBOX_IDS" -eq 1
finish
