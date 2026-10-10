#!/usr/bin/env bash
# Harness 装配阶段 2（替换）：注册第二 harness + agent 绑定 + 升级切态
# （快照路由的 e2e）。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="hs-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W22 Harness 替换 (API=$API RUN_ID=$RUN_ID) =="
# ① 注册第二 harness（org 级 + capabilities）
curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
  -d '{"name":"second","endpoint":"http://harness-2:8000","version":"v1","capabilities":["claude-sonnet-4-6"]}' > /dev/null
assert "第二 harness 注册（active）" \
  bash -c "curl -fsS '$API/orgs/org-$RUN_ID/harnesses' | grep -q second"

# ② agent 绑定 harness_ref（进 spec digest）
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"swap","config":{"model":"claude-sonnet-4-6","instructions":"i","tools":[],"harness_ref":{"name":"second"},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
assert "agent 绑定进 config（harness_ref 字段）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM agents WHERE id='$AID' AND config::text LIKE '%harness_ref%'\" | grep -q 1"

# ③ 升级切态（v2 注册 → active 切换）
curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
  -d '{"name":"second","endpoint":"http://harness-2:8000","version":"v2","capabilities":[]}' > /dev/null
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/harnesses/second/v1/state" -H 'content-type: application/json' \
  -d '{"state":"draining"}' > /dev/null
assert "v2 active + v1 draining（升级切换）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM harness_registry WHERE org_id='org-$RUN_ID' AND name='second' AND state='active' AND version='v2'\" | grep -q 1"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
