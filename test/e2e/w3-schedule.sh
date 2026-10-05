#!/usr/bin/env bash
# W3 验收 e2e ②：定时唤醒——schedule 建计划 → durable timer 到点 → 会话唤醒
# → child run 执行 → 回睡。
# 前置：api/worker 运行中、worker 已注册；harness 默认 fake 模式（无脚本即可）。
# 用法: bash test/e2e/w3-schedule.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="sch-$(date +%s)"
PASS=0; FAIL=0

pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W3 定时唤醒 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

# 1. agent + session + SSE 订阅
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"sch-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是定时唤醒演示助手。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/sch-sse.out 2>&1 &
SSE_PID=$!
sleep 1

# 2. 建一次性唤醒计划（3s 后触发）
SCH=$(curl -fsS -X POST "$API/sessions/$SID/schedules" -H 'content-type: application/json' \
  -d '{"delay_ms":3000,"payload":{"input":"定时唤醒任务"}}')
assert "schedule 创建返回 woken" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d.get("woken") is True and d.get("run_id")' <<< "$SCH"

sleep 1
kill "$SSE_PID" 2>/dev/null || true

# 3. 事件断言：session.woken 出现；child run 完整闭环（run.started 输入=定时唤醒任务 → run.completed）
assert "session.woken 事件（durable timer 到点）" grep -q '"type":"session.woken"' /tmp/sch-sse.out
assert "child run 完成（run.completed）" grep -q '"type":"run.completed"' /tmp/sch-sse.out
assert "child run 输入为 schedule payload" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in sys.stdin if l.startswith("data: ")]; assert any(e["type"]=="run.started" and e["payload"].get("input")=="定时唤醒任务" for e in data)' < /tmp/sch-sse.out

# 4. 回睡：session_object 状态为 paused（控制面读回）
STATE=$(curl -fsS -X POST "http://localhost:8081/session_object/$SID/GetState" -H 'accept: application/json')
assert "会话回睡（phase=paused）" \
  python3 -c 'import sys,json; assert json.load(sys.stdin)["phase"]=="paused"' <<< "$STATE"

echo "  事件序列: $(grep -o '"type":"[a-z._]*"' /tmp/sch-sse.out | paste -sd, -)"
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
