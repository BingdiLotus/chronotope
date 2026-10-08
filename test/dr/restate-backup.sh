#!/usr/bin/env bash
# 期 4 §D3：Restate 状态备份演练——restate 卷快照（tar）→ 卷破坏 → 恢复 →
# 挂起审批（awakeable）可继续 resolve（invocation 状态持久性）。
# 前置：docker compose 全栈运行中 + 一个挂起的 HITL run（本脚本自建）。
# 用法: bash test/dr/restate-backup.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="/opt/homebrew/bin:$PATH"
API="${1:-http://localhost:8080}"
RUN_ID="dr3-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }

echo "== Restate 状态备份演练（期 4 §D3，RUN_ID=${RUN_ID}）=="

# 前置：harness 脚本模式（bash 工具触发 class 2 审批——fake plain 不调工具）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"echo dr3"}}},{"final":"完成。"}]' docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness > /dev/null 2>&1
sleep 5
for i in $(seq 1 20); do curl -fsS localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done

# 1. 建挂起审批（class 2 工具强制审批——api 的 risk 分级）
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"dr-hitl","config":{"model":"claude-sonnet-4-6","instructions":"调用 bash 工具。","tools":["bash"],"tool_classes":{"bash":2},"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
# 写一个 org 审批策略让 bash（class 2）挂起
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/approval-policy" -H 'content-type: application/json' \
  -d '{"approvers":["dr-approver"],"ttl_seconds":600}' > /dev/null 2>&1 || true
curl -fsS -m 10 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"执行 bash"}' > /tmp/dr3-run.out 2>&1 || true
curl -fsS -N -m 30 "$API/sessions/$SID/events?after=0" > /tmp/dr3-sse.out 2>&1 || true
grep -q 'awaiting_approval' /tmp/dr3-sse.out && pass "挂起审批（awakeable 就绪）" || fail "未挂起: $(head -c 150 /tmp/dr3-sse.out)"

# 2. restate 卷快照（tar 全量——演练目标是恢复流程；生产用卷快照/对象存储）
docker run --rm -v chronotope_restatedata:/data -v /tmp:/out alpine sh -c \
  "tar czf /out/dr3-restate.tar.gz -C /data ." > /dev/null 2>&1
pass "restate 卷快照（/tmp/dr3-restate.tar.gz）"

# 3. 破坏：清卷 + 重启 restate（invocation 状态丢失模拟）
docker compose --env-file .env -f deploy/docker-compose.yml stop restate > /dev/null 2>&1
docker run --rm -v chronotope_restatedata:/data alpine sh -c "rm -rf /data/*" > /dev/null 2>&1
docker compose --env-file .env -f deploy/docker-compose.yml start restate > /dev/null 2>&1
sleep 6
for i in $(seq 1 20); do curl -fsS localhost:9070/healthz > /dev/null 2>&1 && break; sleep 1; done
pass "破坏（卷清空 + restate 重启）"

# 4. 恢复：再停 → 卷回填 → 起 → 重新注册 worker 端点
docker compose --env-file .env -f deploy/docker-compose.yml stop restate > /dev/null 2>&1
docker run --rm -v chronotope_restatedata:/data -v /tmp:/out alpine sh -c \
  "rm -rf /data/* && tar xzf /out/dr3-restate.tar.gz -C /data" > /dev/null 2>&1
docker compose --env-file .env -f deploy/docker-compose.yml start restate > /dev/null 2>&1
sleep 6
for i in $(seq 1 20); do curl -fsS localhost:9070/healthz > /dev/null 2>&1 && break; sleep 1; done
curl -fsS -X POST http://localhost:9070/deployments -H 'content-type: application/json' \
  -d '{"uri":"http://worker:9080","version":"dr3-restored","use_http_11":true,"force":true}' > /dev/null 2>&1
sleep 2
pass "卷恢复 + worker 端点重注册"

# 5. 挂起审批可继续 resolve（awakeable 持久性——备份恢复后的 invocation 存活）
RID=$(python3 -c 'import json; t=open("/tmp/dr3-sse.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["run_id"] for e in m if e["type"]=="run.awaiting_approval"][0])' 2>/dev/null || echo "")
DIGEST=$(python3 -c 'import json; t=open("/tmp/dr3-sse.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["payload"].get("action_digest","") for e in m if e["type"]=="run.awaiting_approval"][0])' 2>/dev/null || echo "")
if [ -n "$RID" ]; then
  curl -fsS -m 30 -X POST "$API/webhooks/approval/$RID" -H 'content-type: application/json' \
    -d "{\"payload\":\"approve\",\"action_digest\":\"$DIGEST\",\"approver\":\"dr-approver\"}" > /tmp/dr3-resolve.out 2>&1 || true
  for i in $(seq 1 30); do grep -q 'run.completed' /tmp/dr3-sse.out 2>/dev/null && break; sleep 1; done
  curl -fsS -m 5 -N "$API/sessions/$SID/events?after=0" > /tmp/dr3-sse2.out 2>&1 || true
  grep -q 'run.completed' /tmp/dr3-sse2.out \
    && pass "恢复后挂起审批 resolve → run 完成（awakeable 持久）" \
    || fail "resolve 未使 run 完成（恢复的 invocation 不可达）"
else
  fail "审批 run_id 缺失（run 未挂起或响应形状变化）"
fi

docker run --rm -v /tmp:/out alpine sh -c "rm -f /out/dr3-restate.tar.gz /out/dr3-run.out /out/dr3-sse.out /out/dr3-sse2.out /out/dr3-resolve.out" > /dev/null 2>&1 || true
rm -f /tmp/dr3-run.out /tmp/dr3-sse.out /tmp/dr3-sse2.out /tmp/dr3-resolve.out 2>/dev/null || true
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
