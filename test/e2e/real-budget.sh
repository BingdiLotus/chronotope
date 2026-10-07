#!/usr/bin/env bash
# 真实 e2e：W5 org 预算冻结/解冻（真实用量驱动熔断）。
# ① 设极小预算 → 真实 run 入口冻结（budget.exceeded + run.frozen）
# ② 充值解冻 → 真实模型继续并完成
# 前置：harness 真实模式；worker CONSOLIDATE_THRESHOLD 默认。
# 用法: bash test/e2e/real-budget.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="rbud-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 真实 e2e：org 预算冻结/解冻（API=${API}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-budget-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是预算演示助手，回答一句话。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# 先跑一轮产生真实用量（消耗 token 至超过后续极小预算）
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-0" -d '{"input":"介绍一下你自己"}' > /tmp/rbud-0.out 2>&1 || true

# 预算设 1 token（真实用量远超）→ 新 run 冻结
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/budget" -H 'content-type: application/json' \
  -d '{"daily_token_budget":1}' > /dev/null
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/rbud-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-fz" -d '{"input":"再来一句"}' > /tmp/rbud-fz.out 2>&1 || true
sleep 3
kill "$SSE_PID" 2>/dev/null || true
assert "真实用量触发冻结（budget.exceeded + run.frozen）" \
  bash -c 'grep -q "budget.exceeded" /tmp/rbud-sse.out && grep -q "run.frozen" /tmp/rbud-sse.out'

# 充值解冻
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/budget" -H 'content-type: application/json' \
  -d '{"daily_token_budget":1000000}' > /dev/null
curl -fsS -m 120 -X POST "$API/sessions/$SID/actions" -H 'content-type: application/json' \
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/rbud-uf-sse.out 2>&1 &
UF_SSE=$!
sleep 1
  -d '{"action":"unfreeze"}' > /dev/null 2>&1 || true
sleep 5
assert "解冻后真实模型继续完成（run.completed）" \
  bash -c 'for i in $(seq 1 90); do grep -q "run.completed" /tmp/rbud-uf-sse.out && exit 0; sleep 2; done; exit 1'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
