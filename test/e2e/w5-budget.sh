#!/usr/bin/env bash
# W5 验收 e2e：org 级预算 + 欠费冻结（三级熔断 ②，边界语义 §1）——
# 日预算设极小 → 首轮消费 → 次轮入口检查超限 → run.frozen + budget.exceeded
# （挂起不杀 run）→ 充值（PUT budget）→ unfreeze action → run.unfrozen → 完成。
# 前置：api 以 API_AGGREGATE_INTERVAL=5s 运行（聚合器快速把用量入桶）；
#       harness fake 模式（tokens 4/7 固定）。
# 用法: bash test/e2e/w5-budget.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="bgt-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W5 org 预算 + 欠费冻结 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

ORG="org-$RUN_ID"
AGENT=$(curl -fsS -X POST "$API/orgs/$ORG/agents" -H 'content-type: application/json' \
  -d '{"name":"bgt-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是预算演示助手。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
# org 预算：1 token（首轮 fake 用量 11 即超）；org 由 createAgent 隐式创建
curl -fsS -X PUT "$API/orgs/$ORG/budget" -H 'content-type: application/json' \
  -d '{"daily_token_budget":1}' > /dev/null
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/bgt-sse.out 2>&1 &
SSE_PID=$!
sleep 1

submit() { # $1=input $2=key
  for _ in $(seq 1 30); do
    if curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
      -H "Idempotency-Key: $2" -d "{\"input\":\"$1\"}" > "/tmp/bgt-$2.out" 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# ① 首轮：预算内完成（消耗 11 tokens）
submit "第一轮对话" "$RUN_ID-1"
sleep 8 # 等聚合器（5s 周期）把用量入桶
# ② 次轮：入口检查超限 → 冻结挂起（提交不返回；后台）
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"第二轮对话"}' > /tmp/bgt-2.out 2>&1 &
RUN2_PID=$!
for i in $(seq 1 30); do
  grep -q '"type":"run.frozen"' /tmp/bgt-sse.out 2>/dev/null && break
  sleep 1
done
assert "超限冻结（run.frozen + budget.exceeded，不杀 run）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/bgt-sse.out") if l.startswith("data: ")]; types=[e["type"] for e in data]; assert "budget.exceeded" in types and "run.frozen" in types, types' <<< '{}'

# ③ 充值 → unfreeze → 继续完成
curl -fsS -X PUT "$API/orgs/$ORG/budget" -H 'content-type: application/json' \
  -d '{"daily_token_budget":1000000}' > /dev/null
sleep 1
curl -fsS -X POST "$API/sessions/$SID/actions" -H 'content-type: application/json' \
  -d '{"action":"unfreeze"}' > /dev/null
wait "$RUN2_PID" || true
sleep 1
kill "$SSE_PID" 2>/dev/null || true
assert "充值解冻后继续完成（run.unfrozen → run.completed）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/bgt-sse.out") if l.startswith("data: ")]; types=[e["type"] for e in data]; assert "run.unfrozen" in types and "run.completed" in types, types' <<< '{}'
assert "次轮 run 最终完成" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["final"], d' < /tmp/bgt-2.out

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
