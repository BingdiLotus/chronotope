#!/usr/bin/env bash
# W3 崩溃恢复 chaos：kill -9 api（第 2 步 exec 执行中）→ 重启后 SSE 客户端
# 按 after=seq 断线重连续读，时间轴无缺口、无重复；run 不受影响。
# 用法: bash test/chaos/kill9-api.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

API="${1:-http://localhost:8080}"
RUN_ID="chaos-a-$(date +%s)"
SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo one"}}},{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo two"}}},{"tool_call":{"name":"bash","arguments":{"command":"sleep 2; echo three"}}},{"final":"崩溃恢复完成"}]'

echo "== 崩溃恢复 chaos：kill -9 api（SSE 断线重连 after=seq）=="

stop_local_services; stop_compose_apps
start_harness "$SCRIPT"; start_executor; start_worker; start_api
wait_for_service http://localhost:8000/healthz; wait_for_service http://localhost:9082/healthz; wait_for_service http://localhost:9080/healthz
wait_for_service http://localhost:8080/healthz
register_worker

HARNESS_BASELINE=$(count_of /tmp/chaos-harness.log 'runs start')
EXEC_BASELINE=$(count_of /tmp/chaos-executor.log 'INFO execute')

read -r _ SID <<< "$(seed_agent "$API" '["bash"]')"

# SSE 客户端循环：断线即按 after=cursor 重连续读（追加同一文件）
: > /tmp/chaos-sse.out
sse_loop() {
  local cursor=0
  for _ in $(seq 1 30); do
    curl -fsS -N -m 20 "$API/sessions/$SID/events?after=$cursor" >> /tmp/chaos-sse.out 2>/dev/null || true
    cursor=$(grep -o '"seq":[0-9]*' /tmp/chaos-sse.out | tail -1 | cut -d: -f2)
    cursor="${cursor:-0}"
    grep -q '"type":"run.completed"' /tmp/chaos-sse.out && return 0
    sleep 1
  done
}
sse_loop &
SSE_PID=$!
sleep 1

curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"api 崩溃恢复"}' > /tmp/chaos-run.out &
RUN_PID=$!

wait_for_count /tmp/chaos-sse.out 'sandbox.exec' 1 60
sleep 1 # 第 2 步 exec 执行中
API_PID=$(pgrep -f 'chronotope-api -addr' | head -1)
kill -9 "$API_PID"
echo "  [注入] kill -9 api (pid=${API_PID})，SSE 连接断开"

sleep 1
start_api # 无状态重启；SSE 循环按 after=seq 自动重连
wait "$RUN_PID" || true # 提交 curl 随 api 被杀而断（预期）；run 在 worker 侧继续

# 主信号：以同幂等键重放查询直至 run 完成（契约幂等语义：返回同一 run 当前状态）
RUN=""
for i in $(seq 1 120); do
  RUN=$(curl -fsS -m 5 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
    -H "Idempotency-Key: $RUN_ID" -d '{"input":"api 崩溃恢复"}' 2>/dev/null) || { sleep 1; continue; }
  echo "$RUN" | grep -q '"status":"completed"' && break
  sleep 1
done
wait "$SSE_PID" 2>/dev/null || true
echo "  幂等重放响应: ${RUN}"
assert "run 不受 api 崩溃影响（幂等重放 status=completed）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["idempotent"] is True' <<< "$RUN"

# 时间轴完整性：3×sandbox.exec + run.completed；seq 无重复（无缺口由 after 续读语义保证）
assert "时间轴完整（sandbox.exec ×3）" test "$(count_of /tmp/chaos-sse.out 'sandbox.exec')" -eq 3
assert "run.completed 送达" grep -q '"type":"run.completed"' /tmp/chaos-sse.out
DUPS=$(grep -o '"seq":[0-9]*' /tmp/chaos-sse.out | sort | uniq -d | wc -l | tr -d ' ')
assert "断线重连无重复事件（seq 唯一）" test "$DUPS" -eq 0

# api 崩溃不影响 worker：零重调零重跑
HARNESS_DELTA=$(( $(count_of /tmp/chaos-harness.log 'runs start') - HARNESS_BASELINE ))
EXEC_DELTA=$(( $(count_of /tmp/chaos-executor.log 'INFO execute') - EXEC_BASELINE ))
echo "  harness /runs 增量: ${HARNESS_DELTA}（期望 4）· executor 执行增量: ${EXEC_DELTA}（期望 3）"
assert "worker 侧零重调（/runs=4）" test "$HARNESS_DELTA" -eq 4
assert "沙箱零重跑（execute=3）" test "$EXEC_DELTA" -eq 3
finish
