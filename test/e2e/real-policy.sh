#!/usr/bin/env bash
# 真实 e2e：期 3 §B 审批策略路由（真实模型触发审批 → 策略路由/TTL）。
# ① 真实模型触发 class 2 → 挂起
# ② 非成员批准被拒（audit.approval_denied，仍挂起）
# ③ 成员批准 → 真实完成
# ④ 短 TTL → 过期自动拒绝（audit.approval_expired + tool_denied）
# 前置：harness 真实模式。
# 用法: bash test/e2e/real-policy.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="rpol-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 真实 e2e：审批策略路由（API=${API}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-pol-agent","config":{"model":"claude-sonnet-4-6","instructions":"你必须先调用 bash 工具执行 echo ok，然后一句话回答。","tools":["bash"],"tool_classes":{"bash":2},"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/approval-policy" -H 'content-type: application/json' \
  -d '{"approvers":["alice"],"ttl_seconds":3600}' > /dev/null

SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/rpol-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"执行危险操作"}' > /tmp/rpol-1.out 2>&1 &
RUN_PID=$!
for i in $(seq 1 120); do grep -q '"type":"run.awaiting_approval"' /tmp/rpol-sse.out 2>/dev/null && break; sleep 2; done
assert "真实模型触发审批挂起" grep -q '"type":"run.awaiting_approval"' /tmp/rpol-sse.out
RID=$(python3 -c 'import json; t=open("/tmp/rpol-sse.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["run_id"] for e in m if e["type"]=="run.awaiting_approval"][-1])')
DIGEST=$(python3 -c 'import json; t=open("/tmp/rpol-sse.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["payload"].get("action_digest","") for e in m if e["type"]=="run.awaiting_approval"][-1])')
curl -sS -o /dev/null -X POST "$API/webhooks/approval/$RID" -H 'content-type: application/json' \
  -d "{\"payload\":\"approve\",\"action_digest\":\"$DIGEST\",\"approver\":\"bob\"}"
sleep 3
assert "非成员被拒（audit.approval_denied，仍挂起）" \
  bash -c 'grep -q "audit.approval_denied" /tmp/rpol-sse.out && ! grep -q "run.completed" /tmp/rpol-sse.out'
curl -sS -o /dev/null -X POST "$API/webhooks/approval/$RID" -H 'content-type: application/json' \
  -d "{\"payload\":\"approve\",\"action_digest\":\"$DIGEST\",\"approver\":\"alice\"}"
wait "$RUN_PID" || true
sleep 2
kill "$SSE_PID" 2>/dev/null || true
assert "成员批准后真实完成" python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/rpol-1.out

# ④ TTL 过期
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/approval-policy" -H 'content-type: application/json' \
  -d '{"approvers":["alice"],"ttl_seconds":3}' > /dev/null
SID2=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID2/events?after=0" > /tmp/rpol-ttl.out 2>&1 &
SSE2=$!
sleep 1
curl -fsS -m 300 -X POST "$API/sessions/$SID2/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"再次危险操作"}' > /tmp/rpol-2.out 2>&1 &
RUN2=$!
for i in $(seq 1 120); do grep -q '"type":"run.awaiting_approval"' /tmp/rpol-ttl.out 2>/dev/null && break; sleep 2; done
RID2=$(python3 -c 'import json; t=open("/tmp/rpol-ttl.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["run_id"] for e in m if e["type"]=="run.awaiting_approval"][-1])')
sleep 6
curl -sS -o /dev/null -X POST "$API/webhooks/approval/$RID2" -H 'content-type: application/json' \
  -d '{"payload":"approve","approver":"alice"}'
wait "$RUN2" || true
sleep 2
kill "$SSE2" 2>/dev/null || true
assert "TTL 过期自动拒绝（audit.approval_expired + tool_denied）" \
  bash -c 'grep -q "audit.approval_expired" /tmp/rpol-ttl.out && grep -q "tool_denied" /tmp/rpol-ttl.out'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
