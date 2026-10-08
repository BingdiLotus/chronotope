#!/usr/bin/env bash
# W3 验收 e2e ①：HITL 审批——run 挂起（零进程占用）→ 审批 webhook resolve → 继续执行。
# 前置：api/worker 运行中、worker 已注册；harness 以脚本模式运行（首轮
# request_approval 交棒、次轮终答），脚本轮次循环（见 harness/README）：
#   HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"request_approval","arguments":{"question":"允许删除生产数据吗？"}}},
#   {"final":"已获批准，执行完成。"}]'
# 用法: bash test/e2e/w3-hitl.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="hitl-$(date +%s)"
PASS=0; FAIL=0

pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W3 HITL e2e（API=${API}，RUN_ID=${RUN_ID}）=="

# 1. 建 agent（控制工具 request_approval）+ session
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"hitl-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是审批演示助手。","tools":["request_approval"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# 2. SSE 订阅 + 后台提交任务（会挂起等审批）
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/hitl-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"删除生产数据前请审批"}' > /tmp/hitl-run.out &
RUN_PID=$!

# 3. 等待 awaiting_approval 事件（挂起：零进程占用）
for i in $(seq 1 60); do
  grep -q '"type":"run.awaiting_approval"' /tmp/hitl-sse.out 2>/dev/null && break
  sleep 1
done
assert "run 挂起并发出 run.awaiting_approval 事件" \
  grep -q '"type":"run.awaiting_approval"' /tmp/hitl-sse.out

# 4. 审批回调（webhook resolve awakeable，幂等）
RID=$(grep -o '"run_id":"[^"]*"' /tmp/hitl-sse.out | head -1 | cut -d'"' -f4)
# 审计 #8 后：approver 必填（auth off 自报）+ action_digest 必填（精确绑定）
DIGEST=$(grep -o '"action_digest":"[^"]*"' /tmp/hitl-sse.out | head -1 | cut -d'"' -f4)
RESOLVE=$(curl -fsS -X POST "$API/webhooks/approval/$RID" -H 'content-type: application/json' -d "{\"payload\":\"approve\",\"approver\":\"e2e-approver\",\"action_digest\":\"$DIGEST\"}")
assert "webhook resolve 返回 resolved" python3 -c 'import sys,json; assert json.load(sys.stdin)["status"]=="resolved"' <<< "$RESOLVE"

# 5. run 恢复并完成
wait "$RUN_PID" || true
RUN=$(cat /tmp/hitl-run.out)
assert "run 恢复完成（final 非空）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["final"]' <<< "$RUN"

sleep 1
kill "$SSE_PID" 2>/dev/null || true

# 6. 事件序列：run.started → … → run.awaiting_approval → … → run.resumed → … → run.completed
ORDER=$(grep -o '"type":"run\.[a-z_.]*"' /tmp/hitl-sse.out | paste -sd, -)
echo "  事件序列: $ORDER"
assert "事件含 awaiting_approval 与 resumed（顺序正确）" \
  python3 -c "import sys; s=sys.stdin.read(); assert 'run.awaiting_approval' in s and 'run.resumed' in s and s.index('run.awaiting_approval') < s.index('run.resumed') < s.index('run.completed')" <<< "$ORDER"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
