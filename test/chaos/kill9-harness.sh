#!/usr/bin/env bash
# W3 崩溃恢复 chaos：kill -9 harness（内联 API 工具执行中）→ 重启（无状态即愈），
# worker 按重试策略重发同一 (run_id, step)，恰好一次。
# 场景：step0 是内联 http_request（慢速端点 6s，kill 窗口）→ step1 终答。
# 用法: bash test/chaos/kill9-harness.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

API="${1:-http://localhost:8080}"
RUN_ID="chaos-h-$(date +%s)"
SCRIPT='[{"tool_call":{"name":"http_request","arguments":{"url":"http://127.0.0.1:19077/slow","timeout":8}}},{"final":"harness 崩溃恢复完成"}]'

echo "== 崩溃恢复 chaos：kill -9 harness（内联工具执行中）=="

# 慢速端点：每次连接阻塞 6s 后应答（内联工具的 kill 窗口；可复用 4 次）
python3 - <<'PY' &
import socket, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 19077)); s.listen(4)
for _ in range(4):
    try:
        c, _ = s.accept()
        time.sleep(6)
        c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
        c.close()
    except OSError:
        break
s.close()
PY
SLOW_PID=$!
trap 'kill $SLOW_PID 2>/dev/null || true' EXIT

stop_local_services; stop_compose_apps; ensure_api
start_harness "$SCRIPT"; start_executor; start_worker
wait_for_service http://localhost:8000/healthz; wait_for_service http://localhost:9082/healthz; wait_for_service http://localhost:9080/healthz
register_worker

HARNESS_BASELINE=$(count_of /tmp/chaos-harness.log 'runs start')

read -r _ SID <<< "$(seed_agent "$API" '[]')"
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/chaos-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"harness 崩溃恢复"}' > /tmp/chaos-run.out &
RUN_PID=$!

# 等 step0 的 /runs 开始（内联工具执行中，6s 窗口）→ kill -9 harness
# 注意：uv run 是包装进程，必须杀整组（父 + uvicorn 子），否则子进程存活继续服务
wait_for_count /tmp/chaos-harness.log 'runs start' 1 60
sleep 1
pkill -9 -f 'uvicorn app.main:app'
echo "  [注入] kill -9 harness（uv run 父 + uvicorn 子），内联工具执行中"

sleep 1
start_harness "$SCRIPT" # 无状态：重启即空，同 (run_id, step) 幂等重发
wait_for_service http://localhost:8000/healthz
wait "$RUN_PID" || true
RUN=$(cat /tmp/chaos-run.out)
assert "崩溃后自动续跑完成（steps=1，内联工具场景单次 /runs）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and "harness 崩溃恢复完成" in d["final"] and d["steps"]==1' <<< "$RUN"

sleep 1
kill "$SSE_PID" 2>/dev/null || true
HARNESS_DELTA=$(( $(count_of /tmp/chaos-harness.log 'runs start') - HARNESS_BASELINE ))
STEP0=$(count_of /tmp/chaos-harness.log 'step=0')
STEP1=$(count_of /tmp/chaos-harness.log 'step=1')
echo "  harness /runs 增量: ${HARNESS_DELTA}（期望 2：step0 首次 + 幂等重发一次）"
echo "  step0=${STEP0} step1=${STEP1}"
assert "在途 step 幂等重发恰好一次（step0×2）" test "$STEP0" -eq 2
assert "run.completed" grep -q '"type":"run.completed"' /tmp/chaos-sse.out
finish
