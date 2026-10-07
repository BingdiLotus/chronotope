#!/usr/bin/env bash
# W8 后置 e2e：交付清单（outbox）——run 完成 → 交付行（final/steps/tokens）
# → 投递回执 ack → 状态可见。
# 前置：harness fake 模式；api/worker 运行中。
# 用法: bash test/e2e/w8-delivery.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="dlv-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W8 后置 e2e：交付清单 outbox（API=${API}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"dlv","config":{"model":"claude-sonnet-4-6","instructions":"i","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"完成一个交付"}' > /dev/null

DLV=$(curl -fsS "$API/sessions/$SID/deliveries")
assert "run 完成 → 交付清单一行（kind=run_completed，final 随行）" \
  bash -c 'echo "$1" | grep -q "run_completed" && echo "$1" | grep -q "final" && ! echo "$1" | grep -q "delivered_at"' _ "$DLV"
DID=$(echo "$DLV" | python3 -c 'import sys,json;print(json.load(sys.stdin)["deliveries"][0]["id"])')
assert "投递回执 ack → 204" \
  bash -c 'curl -sS -o /dev/null -w "%{http_code}" -X POST '"$API"'/sessions/'"$SID"'/deliveries/'"$DID"'/ack | grep -q 204'
assert "ack 后清单状态可见（delivered_at 非空）" \
  bash -c 'curl -fsS '"$API"'/sessions/'"$SID"'/deliveries | python3 -c "import sys,json; d=json.load(sys.stdin)[\"deliveries\"][0]; assert d[\"delivered_at\"] is not None, d"'

# journal 审计导出（正式版架构 期 1）：重放轨迹 + dedupe 证据链
RID=$(curl -fsS "$API/sessions/$SID/deliveries" | python3 -c 'import sys,json;print(json.load(sys.stdin)["deliveries"][0]["run_id"])')
AUDIT=$(curl -fsS "$API/runs/$RID/audit")
assert "journal 审计导出（run.started → run.completed 轨迹 + dedupe 键）" \
  bash -c 'echo "$1" | grep -q "run.started" && echo "$1" | grep -q "run.completed" && echo "$1" | grep -q "dedupe_key"' _ "$AUDIT"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
