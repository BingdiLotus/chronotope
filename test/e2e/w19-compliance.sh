#!/usr/bin/env bash
# M3 合规验收 demo：长周期带审批的资金/工单类 agent 全链路——
# Goal→WorkItem→Claim→执行→审批 Gate→Evidence→审计导出+链验证。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="cmp-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== M3 compliance demo (API=$API RUN_ID=$RUN_ID) =="
# 0 先建 agent（org 隐式创建——goal 的 FK 地基）
curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"seed","config":{"model":"m","instructions":"i","tools":[],"version":1}}' > /dev/null

# ① Goal（慢变量 + version）
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/goals/g1" -H 'content-type: application/json' \
  -d '{"objective":"处理退款工单：为回调补充幂等键","scope":"payment-refund","owner":"ops"}' > /tmp/cmp-goal.out
assert "Goal 创建（version 1 + state_hash）" \
  python3 -c 'import sys,json; d=json.load(open("/tmp/cmp-goal.out")); assert d["version"]>=1 and d["state_hash"], d' <<< '{}'

# ② WorkItem 切片
curl -fsS -X POST "$API/orgs/org-$RUN_ID/goals/g1/items" -H 'content-type: application/json' \
  -d '{"id":"wi-refund-1","description":"为退款回调补充幂等键并让集成测试通过","priority":1,"task_class":"code"}' > /dev/null
assert "WorkItem 可领取切片" \
  python3 -c 'import sys,json,urllib.request
d=json.load(urllib.request.urlopen("'$API'/orgs/org-'$RUN_ID'/goals/g1/items"))
assert any(i["id"]=="wi-refund-1" for i in d["items"]), d' <<< '{}'
# ③ 执行（带审批 Gate——class 2 bash）+ 批准
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"refund-agent","config":{"model":"m","instructions":"你是退款工单处理助手。","tools":["bash"],"tool_classes":{"bash":2},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"echo idempotency-key-added"}}},{"final":"退款回调幂等键已补充。"}]' \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness >/dev/null 2>&1
sleep 4
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/cmp-sse.out 2>&1 &
SSE=$!
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"处理退款回调"}' > /tmp/cmp-run.out 2>&1 &
RUNP=$!
for i in $(seq 1 60); do grep -q '"type":"run.awaiting_approval"' /tmp/cmp-sse.out 2>/dev/null && break; sleep 2; done
assert "审批 Gate 挂起（action_digest 绑定）" \
  grep -q '"type":"run.awaiting_approval"' /tmp/cmp-sse.out
DIGEST=$(python3 -c 'import json; t=open("/tmp/cmp-sse.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["payload"].get("action_digest","") for e in m if e["type"]=="run.awaiting_approval"][-1])')
curl -fsS -X POST "$API/webhooks/approval/$(grep -o '"run_id":"[^"]*"' /tmp/cmp-sse.out | head -1 | cut -d'"' -f4)" \
  -H 'content-type: application/json' \
  -d "{\"payload\":\"approve\",\"approver\":\"ops\",\"action_digest\":\"$DIGEST\"}" > /dev/null
wait "$RUNP" || true
sleep 2
assert "批准后执行完成（effect ledger 落账）" \
  python3 -c 'import sys,json; d=json.load(open("/tmp/cmp-run.out")); assert d["status"]=="completed", d' <<< '{}'

# ④ Evidence（新鲜度 + source_revision）
curl -fsS -X POST "$API/sessions/$SID/evidence" -H 'content-type: application/json' \
  -d '{"id":"ev-refund-1","run_id":"r1","blob_hash":"abc123","valid_for":"commit-xyz","source_revision":"rev-1","method":"integration-test"}' > /dev/null
assert "Evidence 记录（valid_for/source_revision/method）" \
  bash -c 'docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "SELECT count(*) FROM evidence WHERE id='"'"'ev-refund-1'"'"'" | grep -q 1' ''

# ⑤ 审计导出 + 链验证（合规的核心断言）
curl -fsS "$API/sessions/$SID/audit-export" > /tmp/cmp-audit.json
assert "审计链独立验证通过（历史未改写）" \
  bash scripts/audit-verify.sh /tmp/cmp-audit.json
kill $SSE 2>/dev/null || true

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
