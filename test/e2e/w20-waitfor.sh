#!/usr/bin/env bash
# 期 6 ① WaitFor + intent：timer 等待的注册/解决 + operator_input 审批的
# intent 落账（两条覆盖真实场景——「为什么停」的可判定与可问责）。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="wf-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }
PSQL() { docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "$1"; }

echo "== W20 WaitFor + intent (API=$API RUN_ID=$RUN_ID) =="
# ① timer 等待（schedule 触发——注册与解决）
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"w20","config":{"model":"m","instructions":"i","tools":[],"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"final":"ok"}]' \
  docker compose -f deploy/docker-compose.yml up -d --force-recreate harness >/dev/null 2>&1
sleep 4
curl -fsS -X POST "$API/sessions/$SID/schedules" -H 'content-type: application/json' \
  -d "{\"delay_ms\":6000,\"payload\":{\"input\":\"hi\",\"schedule_id\":\"$RUN_ID-sch\"}}" >/dev/null
sleep 4
assert "timer 等待注册（kind=timer + intent 定时唤醒）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM session_waits WHERE session_id='$SID' AND kind='timer' AND intent='定时唤醒'\" | grep -q 1"
sleep 8
assert "timer 等待解决（唤醒后 resolved_at 落账）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM session_waits WHERE session_id='$SID' AND kind='timer' AND resolved_at IS NOT NULL\" | grep -q 1"

# ② operator_input 审批等待（intent/expect 落账——「为什么停」可审计）
AID2=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"w20-ap","config":{"model":"m","instructions":"i","tools":["bash"],"tool_classes":{"bash":2},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID2=$(curl -fsS -X POST "$API/agents/$AID2/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"echo x"}}},{"final":"done"}]' \
  docker compose -f deploy/docker-compose.yml up -d --force-recreate harness >/dev/null 2>&1
sleep 4
curl -fsS -m 30 -X POST "$API/sessions/$SID2/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-ap-1" -d '{"input":"do it"}' > /dev/null 2>&1 || true
sleep 4
assert "operator_input 等待注册（intent=审批请求 + expect 条件）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM session_waits WHERE session_id='$SID2' AND kind='operator_input' AND intent LIKE '%审批请求%'\" | grep -q 1"

# ② DECIDE 可判定：长 timer 等待（30s）期间第二个 schedule 到点——decide
# 查活跃 timer 等待 → wait（在等什么不是空转——启发式误判 quiet 的反例关闭）
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/wf-sse2.out 2>&1 &
SSE2=$!
curl -sS -m 5 -X POST "$API/sessions/$SID/schedules" -H 'content-type: application/json' \
  -d "{\"delay_ms\":30000,\"payload\":{\"input\":\"long\",\"schedule_id\":\"$RUN_ID-long\"}}" >/dev/null 2>&1 || true
sleep 2
curl -sS -m 5 -X POST "$API/sessions/$SID/schedules" -H 'content-type: application/json' \
  -d "{\"delay_ms\":2000,\"payload\":{\"input\":\"check\",\"schedule_id\":\"$RUN_ID-decide\"}}" >/dev/null 2>&1 || true
sleep 6
kill $SSE2 2>/dev/null || true
assert "DECIDE 可判定（活跃 timer 等待 → decide 事件 hint=wait 非 quiet）" \
  grep -q '"hint":"wait"' /tmp/wf-sse2.out

# 注：causal slice 的等待意图段由单测覆盖（TestBuildMessagesWaitIntent——
# e2e 的 harness 日志不打请求体）

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
