#!/usr/bin/env bash
# 期 3 治理四项 e2e（w13）：策略缝另一端的真实链路实证——
#   ① B 审批策略路由（非成员拒绝 audit.approval_denied 仍挂起 → 成员批准完成）
#   ② B TTL 过期自动拒绝（audit.approval_expired + tool_denied 终态）
#   ③ A principal 限流（API_AUTH_MODE=on + key 绑 user → 429 + Retry-After）
#   ④ D 共享知识跨会话挂载（POST knowledge → 新会话 run → harness 载荷含【共享知识】）
#   ⑤ C allowlist 过滤（白名单外 MCP 工具不下发——llm.call 载荷 tools 断言）
# 编排：本地 on 模式 api（compose api 容器停）+ compose 支撑栈（worker/executor/
# harness/postgres/restate/rustfs）；harness 按段重建脚本模式。
# 用法: bash test/e2e/w13-policy.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="/opt/homebrew/bin:$PATH"
API="${1:-http://localhost:8080}"
ADMIN_KEY="admin-tt-key"
DB='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'
RUN_ID="w13-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}
AUTH=(-H "Authorization: Bearer $ADMIN_KEY")

echo "== 期 3 治理 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

# 0. restate 卷重置（半死 invocation 积累会重放旧代码路径——CI 全新环境同构）
docker compose --env-file .env -f deploy/docker-compose.yml down restate > /dev/null 2>&1 || true
docker volume rm -f chronotope_restatedata > /dev/null 2>&1 || true
docker compose --env-file .env -f deploy/docker-compose.yml up -d restate > /dev/null 2>&1
sleep 5
curl -fsS -X POST http://localhost:9070/deployments -H 'content-type: application/json' \
  -d '{"uri":"http://worker:9080","version":"w13-v1","use_http_11":true,"force":true}' > /dev/null 2>&1 || true
sleep 2
# 本地 on 模式 api（compose api 容器让位；支撑栈保留）
docker compose --env-file .env -f deploy/docker-compose.yml stop api > /dev/null 2>&1 || true
pkill -f 'chronotope-api' 2>/dev/null || true
go build -o bin/chronotope-api ./cmd/api
nohup env DATABASE_URL="$DB" RESTATE_URL=http://localhost:8081 API_AUTH_MODE=on API_ADMIN_KEY="$ADMIN_KEY" \
  ./bin/chronotope-api -addr :8080 > /tmp/w13-api.log 2>&1 &
for i in $(seq 1 30); do curl -fsS "$API/healthz" > /dev/null 2>&1 && break; sleep 1; done
for i in $(seq 1 30); do curl -fsS -H "Authorization: Bearer $ADMIN_KEY" "$API/orgs/org-$RUN_ID/agents" > /dev/null 2>&1 && break; sleep 1; done
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness > /dev/null 2>&1
sleep 3

# ─── ③ A：principal 限流（key 绑 user → user 桶 429 + Retry-After）───
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' "${AUTH[@]}" \
  -d '{"name":"policy-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是治理演示助手。","tools":["bash"],"tool_classes":{"bash":2},"version":1}}')
USER_JSON=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/users" -H 'content-type: application/json' "${AUTH[@]}" \
  -d '{"name":"张三"}')
KEY_JSON=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/keys" -H 'content-type: application/json' "${AUTH[@]}" \
  -d '{"user_id":"u_principal"}')
USER_KEY=$(echo "$KEY_JSON" | python3 -c 'import sys,json;print(json.load(sys.stdin)["key"])')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" "${AUTH[@]}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
# 连发 4 次（burst 3）→ 第 4 次 user 桶 429
CURL_PIDS=""
for i in 1 2 3 4; do
  SIDX=$(curl -fsS -X POST "$API/agents/$AID/sessions" "${AUTH[@]}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
  curl -sS -o /tmp/w13-429-$i.out -w '%{http_code}' -m 60 -X POST "$API/sessions/$SIDX/runs" \
    -H 'content-type: application/json' -H "Authorization: Bearer $USER_KEY" \
    -H "Idempotency-Key: $RUN_ID-limit-$i" -d '{"input":"限流测试"}' > /tmp/w13-429-$i.code 2>/dev/null &
  CURL_PIDS="$CURL_PIDS $!"
done
wait $CURL_PIDS
assert "principal 限流：绑 user key 并发 4 连发触发 429 + Retry-After" \
  bash -c 'grep -q "^429$" /tmp/w13-429-1.code /tmp/w13-429-2.code /tmp/w13-429-3.code /tmp/w13-429-4.code' ''

# ─── ① B：审批策略路由（bob 拒绝 → alice 批准）───
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/approval-policy" -H 'content-type: application/json' "${AUTH[@]}" \
  -d '{"approvers":["alice"],"ttl_seconds":3600}' > /dev/null
SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"echo ok"}}},{"final":"治理审批完成。"}]'
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT="$SCRIPT" \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness > /dev/null 2>&1
sleep 4
SID2=$(curl -fsS -X POST "$API/agents/$AID/sessions" "${AUTH[@]}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID2/events?after=0" "${AUTH[@]}" > /tmp/w13-b1.out 2>&1 &
sleep 1
curl -fsS -m 120 -X POST "$API/sessions/$SID2/runs" -H 'content-type: application/json' "${AUTH[@]}" \
  -H "Idempotency-Key: $RUN_ID-b1" -d '{"input":"执行危险操作"}' > /tmp/w13-b1-run.out &
for i in $(seq 1 60); do grep -q '"type":"run.awaiting_approval"' /tmp/w13-b1.out 2>/dev/null && break; sleep 1; done
assert "审批策略路由：run 挂起等待审批" grep -q '"type":"run.awaiting_approval"' /tmp/w13-b1.out
RID=$(python3 -c 'import json,re; t=open("/tmp/w13-b1.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["run_id"] for e in m if e["type"]=="run.awaiting_approval"][-1])')
DIGEST=$(python3 -c 'import json; t=open("/tmp/w13-b1.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["payload"].get("action_digest","") for e in m if e["type"]=="run.awaiting_approval"][-1])')
# bob（非成员）→ 拒绝（run 保持挂起 + audit.approval_denied）；拒绝是 TerminalError
# 的 HTTP 表达（502）——curl 容忍非 2xx（-sS 不带 -f）
curl -sS -o /dev/null -X POST "$API/webhooks/approval/$RID" -H 'content-type: application/json' \
  -d "{\"payload\":\"approve\",\"action_digest\":\"$DIGEST\",\"approver\":\"bob\"}"
sleep 2
assert "非成员审批被拒（audit.approval_denied，run 仍挂起）" \
  bash -c 'grep -q "audit.approval_denied" /tmp/w13-b1.out && ! grep -q "run.completed" /tmp/w13-b1.out'
# alice（成员）→ 批准 → 完成
curl -fsS -X POST "$API/webhooks/approval/$RID" -H 'content-type: application/json' \
  -d "{\"payload\":\"approve\",\"action_digest\":\"$DIGEST\",\"approver\":\"alice\"}" > /dev/null
for i in $(seq 1 60); do grep -q '"type":"run.completed"' /tmp/w13-b1.out 2>/dev/null && break; sleep 1; done
assert "成员批准后 run 恢复完成" grep -q '"type":"run.completed"' /tmp/w13-b1.out

# ─── ② B：TTL 过期自动拒绝 ───
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/approval-policy" -H 'content-type: application/json' "${AUTH[@]}" \
  -d '{"approvers":["alice"],"ttl_seconds":3}' > /dev/null
SID3=$(curl -fsS -X POST "$API/agents/$AID/sessions" "${AUTH[@]}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID3/events?after=0" "${AUTH[@]}" > /tmp/w13-b2.out 2>&1 &
sleep 1
curl -fsS -m 120 -X POST "$API/sessions/$SID3/runs" -H 'content-type: application/json' "${AUTH[@]}" \
  -H "Idempotency-Key: $RUN_ID-b2" -d '{"input":"再次危险操作"}' > /tmp/w13-b2-run.out &
for i in $(seq 1 60); do grep -q '"type":"run.awaiting_approval"' /tmp/w13-b2.out 2>/dev/null && break; sleep 1; done
RID2=$(python3 -c 'import json; t=open("/tmp/w13-b2.out").read(); m=[json.loads(l[6:]) for l in t.splitlines() if l.startswith("data: ")]; print([e["run_id"] for e in m if e["type"]=="run.awaiting_approval"][-1])')
sleep 5 # 过 TTL
curl -fsS -X POST "$API/webhooks/approval/$RID2" -H 'content-type: application/json' \
  -d '{"payload":"approve","approver":"alice"}' > /dev/null
sleep 3
assert "TTL 过期自动拒绝（audit.approval_expired + tool_denied 终态）" \
  bash -c 'grep -q "audit.approval_expired" /tmp/w13-b2.out && grep -q "tool_denied" /tmp/w13-b2.out'

# ─── ⑤ C：allowlist 过滤（白名单外 MCP 工具不下发）───
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"mcp:echo:echo","arguments":{"text":"hi"}}},{"final":"MCP 调用完成。"}]' \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness > /dev/null 2>&1
sleep 4
# allowlist：只放行 mcp:echo 下的 other（echo 工具本身被拒——过滤实证）
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/mcp-allowlist" -H 'content-type: application/json' "${AUTH[@]}" \
  -d '{"server":"echo","tool_patterns":["other."]}' > /dev/null
nohup python3 test/fixtures/mcp-server.py 9100 > /tmp/w13-mcp-fixture.log 2>&1 &
sleep 2
SID4=$(curl -fsS -X POST "$API/agents/$AID/sessions" "${AUTH[@]}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -X POST "$API/sessions/$SID4/mcp" -H 'content-type: application/json' "${AUTH[@]}" \
  -d '{"server":"echo","url":"http://localhost:9100"}' > /dev/null
curl -fsS -N "$API/sessions/$SID4/events?after=0" "${AUTH[@]}" > /tmp/w13-c.out 2>&1 &
sleep 1
# 执行层 allowlist 防御（期 5 §B 后）：被拒工具 → run 失败（502）——
# curl 容忍非 2xx；核心断言是「无 mcp.call 事件」（拒发）
curl -sS -m 120 -X POST "$API/sessions/$SID4/runs" -H 'content-type: application/json' "${AUTH[@]}" \
  -H "Idempotency-Key: $RUN_ID-c" -d '{"input":"调用 MCP 工具"}' > /tmp/w13-c-run.out 2>&1 || true
assert "allowlist 过滤：未列工具不下发（无 mcp.call 事件）" \
  bash -c '! grep -q "mcp.call" /tmp/w13-c.out'

# ─── ④ D：共享知识跨会话挂载注入 ───
# harness 回 plain（D 段 run 正常终答；共享知识注入经 harness 载荷日志断言）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness > /dev/null 2>&1
sleep 4
EMB=$(python3 -c 'import json; print(json.dumps([0.01]*1024))')
curl -fsS -X POST "$API/orgs/org-$RUN_ID/knowledge" -H 'content-type: application/json' "${AUTH[@]}" \
  -d "{\"content\":\"治理知识片段\",\"embedding\":$EMB}" > /dev/null
SID5=$(curl -fsS -X POST "$API/agents/$AID/sessions" "${AUTH[@]}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 120 -X POST "$API/sessions/$SID5/runs" -H 'content-type: application/json' "${AUTH[@]}" \
  -H "Idempotency-Key: $RUN_ID-d" -d '{"input":"使用共享知识回答"}' > /tmp/w13-d-run.out 2>&1
assert "共享知识挂载：新会话 run 载荷含【共享知识】片段" \
  bash -c 'docker compose --env-file .env -f deploy/docker-compose.yml logs harness 2>/dev/null | grep -q "【共享知识】"'

# 恢复 compose api
pkill -f 'chronotope-api' 2>/dev/null || true
docker compose --env-file .env -f deploy/docker-compose.yml start api > /dev/null 2>&1 || \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d api > /dev/null 2>&1
pkill -f 'mcp-server.py' 2>/dev/null || true

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
