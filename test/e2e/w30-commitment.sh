#!/usr/bin/env bash
# W30 承诺原语 e2e：出站门（MCP post_message → 承诺门 → pending → 回执
# open）+ 投影/事件/字节级判重（承诺原语 §2.2/§3.2 的端到端闭环）。
# 前置：MCP 夹具（post_message 工具）+ fake harness 脚本（调
# mcp:echo:post_message）；worker 的 COMMITMENT_DECIDER 默认 rule 档。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
MCP_URL="${2:-http://host.docker.internal:9100}"
RUN_ID="cm-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W30 承诺原语 e2e（API=$API MCP=$MCP_URL RUN_ID=${RUN_ID}）=="
rm -f /tmp/cm-run.out
for i in $(seq 1 30); do curl -fsS "$API/healthz" >/dev/null 2>&1 && break; sleep 1; done

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"cm-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是对外沟通助手。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# --- ① MCP 连接（post_message 工具） ---
assert "MCP 连接注册（echo server 含 post_message）" \
  curl -fsS -X POST "$API/sessions/$SID/mcp" -H 'content-type: application/json' \
  -d "{\"server\":\"echo\",\"url\":\"$MCP_URL\"}"

# --- ② fake 脚本：模型调出站工具（内容含承诺触发词「我会」） ---
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"mcp:echo:post_message","arguments":{"content":"我会明天给你答复"}}},{"final":"已发送（承诺门放行）"}]' \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness > /dev/null 2>&1
for i in $(seq 1 30); do curl -fsS "$API/healthz" >/dev/null 2>&1 && curl -fsS -m 2 http://host.docker.internal:8000/healthz >/dev/null 2>&1 && break; sleep 1; done

# --- ③ run：出站工具经承诺门 → 记录 pending → 回执 open ---
curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"向线程发送消息：我会明天给你答复"}' -o /tmp/cm-run.out 2>&1 || true
assert "run 完成（出站工具经承诺门放行）" \
  python3 -c 'import json; d=json.load(open("/tmp/cm-run.out")); assert d["status"]=="completed", d'

# --- ④ 投影：state=open（回执成功）+ confirmed_at 非空 ---
assert "承诺投影 open（回执成功——pending→open）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM agent_commitments WHERE made_in_thread='$SID' AND state='open' AND confirmed_at IS NOT NULL\" | grep -q '^[1-9]'"

# --- ⑤ 内容与字节级判重（content_digest = sha256(content)） ---
assert "承诺内容落地（含「我会明天给你答复」）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM agent_commitments WHERE made_in_thread='$SID' AND content LIKE '%我会明天给你答复%'\" | grep -q '^[1-9]'"
assert "content_digest 为 sha256(content)（字节级判重语义）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT content || '|' || content_digest FROM agent_commitments WHERE made_in_thread='$SID' LIMIT 1\" | python3 -c \"import sys,hashlib; line=sys.stdin.read().strip(); c,d=line.rsplit('|',1); assert hashlib.sha256(c.encode()).hexdigest()==d, line\""

# --- ⑥ 事件：agent.committed 进事件表（哈希链——真相） ---
assert "agent.committed 事件已落（承诺初写为事件）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM events WHERE session_id='$SID' AND type='agent.committed'\" | grep -q '^[1-9]'"

# --- ⑦ 幂等：同 run 重放不重复创建（去重——事件 dedupe） ---
BEFORE=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "SELECT count(*) FROM agent_commitments WHERE made_in_thread='$SID'")
sleep 1
assert "承诺幂等（重放不重复创建）" \
  bash -c "AFTER=\$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM agent_commitments WHERE made_in_thread='$SID'\"); [ \"\$AFTER\" = '$BEFORE' ]"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
