#!/usr/bin/env bash
# W5 验收 e2e：工具风险分级 class 2 强制审批（边界语义 §2）——
# ① 批准路径：class 2 bash → 挂起 → webhook 批准 → 执行 → 完成；
# ② 拒绝路径：class 2 bash → webhook 拒绝 → audit.tool_denied + run.failed{tool_denied}，不执行。
# 前置：api/worker/harness 运行中（harness fake 脚本模式产出 bash 工具调用）。
# 用法: bash test/e2e/w5-risk.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="risk-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W5 工具风险分级 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

make_agent() { # $1=name → AID（tools=[bash] + tool_classes{bash:2}）
  local resp
  resp=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
    -d "{\"name\":\"$1\",\"config\":{\"model\":\"claude-sonnet-4-6\",\"instructions\":\"你是风险分级演示助手。\",\"tools\":[\"bash\"],\"tool_classes\":{\"bash\":2},\"version\":1}}")
  echo "$resp" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
}

submit_bg() { # $1=SID $2=key → 后台提交（挂起中），输出 run_id
  curl -fsS -m 120 -X POST "$API/sessions/$1/runs" -H 'content-type: application/json' \
    -H "Idempotency-Key: $2" -d '{"input":"执行危险命令"}' > "/tmp/risk-$2.out" 2>&1 &
  echo $!
}

# --- ① 批准路径 ---
AID=$(make_agent risk-agent)
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/risk-approve-sse.out 2>&1 &
SSE1=$!
sleep 1
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-ap" -d '{"input":"执行危险命令"}' > /tmp/risk-ap.out 2>&1 &
RUN1=$!
# 等挂起事件（class 2 强制审批）
for i in $(seq 1 30); do
  grep -q '"type":"run.awaiting_approval"' /tmp/risk-approve-sse.out 2>/dev/null && break
  sleep 1
done
assert "class 2 强制挂起（run.awaiting_approval + risk_class=2）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/risk-approve-sse.out") if l.startswith("data: ")]; ev=[e for e in data if e["type"]=="run.awaiting_approval"]; assert ev and ev[0]["payload"]["risk_class"]==2' <<< '{}'
RID1=$(grep -a -B2 "awakeable_id" /tmp/risk-approve-sse.out | grep -a -o '"run_id":"[^"]*"' | head -1 | cut -d'"' -f4)
curl -fsS -X POST "$API/webhooks/approval/$RID1" -H 'content-type: application/json' \
  -d '{"payload":"已批准"}' > /dev/null
wait "$RUN1" || true
sleep 1 # 事件经 poller 送达（与提交 curl 竞态）
assert "批准后 bash 执行（sandbox.exec exit 0 + run.completed）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/risk-approve-sse.out") if l.startswith("data: ")]; types=[e["type"] for e in data]; assert "sandbox.exec" in types and "run.completed" in types, types' <<< '{}'
kill "$SSE1" 2>/dev/null || true

# --- ② 拒绝路径 ---
AID2=$(make_agent risk-agent-deny)
SID2=$(curl -fsS -X POST "$API/agents/$AID2/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID2/events?after=0" > /tmp/risk-deny-sse.out 2>&1 &
SSE2=$!
sleep 1
curl -fsS -m 120 -X POST "$API/sessions/$SID2/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-dn" -d '{"input":"执行危险命令"}' > /tmp/risk-dn.out 2>&1 &
RUN2=$!
for i in $(seq 1 30); do
  grep -q '"type":"run.awaiting_approval"' /tmp/risk-deny-sse.out 2>/dev/null && break
  sleep 1
done
RID2=$(grep -a -B2 "awakeable_id" /tmp/risk-deny-sse.out | grep -a -o '"run_id":"[^"]*"' | head -1 | cut -d'"' -f4)
curl -fsS -X POST "$API/webhooks/approval/$RID2" -H 'content-type: application/json' \
  -d '{"payload":"{\"approved\":false,\"note\":\"拒绝危险操作\"}"}' > /dev/null
wait "$RUN2" || true
sleep 1 # 事件经 poller 送达（提交 curl 与事件流竞态）
assert "拒绝 → audit.tool_denied + run.failed{tool_denied}" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/risk-deny-sse.out") if l.startswith("data: ")]; types=[e["type"] for e in data]; failed=[e for e in data if e["type"]=="run.failed"]; assert "audit.tool_denied" in types and failed and failed[0]["payload"]["reason"]=="tool_denied", (types, failed)' <<< '{}'
assert "拒绝后 bash 不执行（无 sandbox.exec）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/risk-deny-sse.out") if l.startswith("data: ")]; assert not any(e["type"]=="sandbox.exec" for e in data)' <<< '{}'
kill "$SSE2" 2>/dev/null || true

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
