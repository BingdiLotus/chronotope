#!/usr/bin/env bash
# W3 崩溃恢复 chaos：Restate 引擎重启（第 2 步 exec 执行中）→ 日志持久化透明恢复，
# 在途 step 幂等重发恰好一次。
# 用法: bash test/chaos/kill9-restate.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

API="${1:-http://localhost:8080}"
RUN_ID="chaos-r-$(date +%s)"
SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo one"}}},{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo two"}}},{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo three"}}},{"final":"崩溃恢复完成"}]'

echo "== 崩溃恢复 chaos：Restate 引擎重启（第 2 步 exec 执行中）=="

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
curl -fsS -m 240 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"Restate 引擎重启"}' > /tmp/chaos-run.out &
RUN_PID=$!

wait_for_count /tmp/chaos-sse.out 'sandbox.exec' 1 60
sleep 1 # 第 2 步 exec 执行中
docker restart chronotope-restate-1 > /dev/null
echo "  [注入] docker restart restate（invocation 日志持久化于数据卷）"

wait "$RUN_PID" || true # 提交 curl 随引擎重启而断（预期）；run 由 journal 恢复续跑

# 主信号：同幂等键重放轮询直至 completed（api 幂等语义 + worker 终态记账）
RUN=""
for i in $(seq 1 150); do
  RUN=$(curl -fsS -m 5 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
    -H "Idempotency-Key: $RUN_ID" -d '{"input":"Restate 引擎重启"}' 2>/dev/null) || { sleep 1; continue; }
  echo "$RUN" | grep -q '"status":"completed"' && break
  sleep 1
done
echo "  幂等重放响应: ${RUN}"
assert "引擎重启后自动续跑完成（status=completed）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["idempotent"] is True' <<< "$RUN"

sleep 1
kill "$SSE_PID" 2>/dev/null || true
assert "sandbox.exec ×3" test "$(count_of /tmp/chaos-sse.out 'sandbox.exec')" -eq 3

HARNESS_DELTA=$(( $(count_of /tmp/chaos-harness.log 'runs start') - HARNESS_BASELINE ))
EXEC_DELTA=$(( $(count_of /tmp/chaos-executor.log 'INFO execute') - EXEC_BASELINE ))
echo "  harness /runs 增量: ${HARNESS_DELTA}（期望 4）· executor 执行增量: ${EXEC_DELTA}（期望 4）"
assert "已完成 step 零重调（/runs=4）" test "$HARNESS_DELTA" -eq 4
assert "在途 step 重发恰好一次（execute=4）" test "$EXEC_DELTA" -eq 4
finish
