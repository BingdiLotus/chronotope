#!/usr/bin/env bash
# 真实 e2e：W3 HITL + W5 风险分级（真实模型触发 class 2 审批 → 批准/拒绝）。
# ① 真实模型主动调用 bash（class 2 强制审批）→ run 挂起
# ② 拒绝 → tool_denied 终态（真实模型收到拒绝）
# ③ 批准 → 真实模型继续执行并完成
# 前置：harness 真实模式。
# 用法: bash test/e2e/real-hitl.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="rhitl-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 真实 e2e：HITL 审批 + 风险分级（API=${API}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-hitl-agent","config":{"model":"claude-sonnet-4-6","instructions":"你必须先调用 bash 工具执行 echo hi，然后根据工具结果回答。","tools":["bash"],"tool_classes":{"bash":2},"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# --- ①+③ 批准链路 ---
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/rhitl-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-ap" -d '{"input":"执行危险操作"}' > /tmp/rhitl-ap.out 2>&1 &
RUN_PID=$!
for i in $(seq 1 120); do grep -q '"type":"run.awaiting_approval"' /tmp/rhitl-sse.out 2>/dev/null && break; sleep 2; done
assert "真实模型触发 class 2 审批（run.awaiting_approval）" \
  grep -q '"type":"run.awaiting_approval"' /tmp/rhitl-sse.out
RID=$(python3 -c 'import json; t=open("/tmp/rhitl-sse.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["run_id"] for e in m if e["type"]=="run.awaiting_approval"][-1])')
DIGEST=$(python3 -c 'import json; t=open("/tmp/rhitl-sse.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["payload"].get("action_digest","") for e in m if e["type"]=="run.awaiting_approval"][-1])')
curl -sS -o /dev/null -X POST "$API/webhooks/approval/$RID" -H 'content-type: application/json' \
  -d "{\"payload\":\"approve\",\"action_digest\":\"$DIGEST\",\"approver\":\"human\"}"
wait "$RUN_PID" || true
sleep 2
kill "$SSE_PID" 2>/dev/null || true
assert "批准后真实模型完成（引用工具结果）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/rhitl-ap.out

# --- ② 拒绝链路 ---
SID2=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID2/events?after=0" > /tmp/rhitl-dn.out 2>&1 &
SSE2=$!
sleep 1
curl -fsS -m 300 -X POST "$API/sessions/$SID2/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-dn" -d '{"input":"执行危险操作"}' > /tmp/rhitl-dn-run.out 2>&1 &
RUN2=$!
for i in $(seq 1 120); do grep -q '"type":"run.awaiting_approval"' /tmp/rhitl-dn.out 2>/dev/null && break; sleep 2; done
RID2=$(python3 -c 'import json; t=open("/tmp/rhitl-dn.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["run_id"] for e in m if e["type"]=="run.awaiting_approval"][-1])')
DIGEST2=$(python3 -c 'import json; t=open("/tmp/rhitl-dn.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["payload"].get("action_digest","") for e in m if e["type"]=="run.awaiting_approval"][-1])')
curl -sS -o /dev/null -X POST "$API/webhooks/approval/$RID2" -H 'content-type: application/json' \
  -d "{\"payload\":$(python3 -c 'import json;print(json.dumps(json.dumps({"approved":False,"note":"人工拒绝"})))'),\"action_digest\":\"$DIGEST2\",\"approver\":\"human\"}"
wait "$RUN2" || true
sleep 2
kill "$SSE2" 2>/dev/null || true
assert "拒绝后 tool_denied 终态（工具未执行）" \
  bash -c 'grep -q "audit.tool_denied" /tmp/rhitl-dn.out && ! grep -q "sandbox.exec" /tmp/rhitl-dn.out'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
