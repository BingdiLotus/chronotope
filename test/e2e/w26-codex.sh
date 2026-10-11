#!/usr/bin/env bash
# 批 2 Codex 官方 SDK 接入 e2e：注册表 + 绑定 → 真实 Codex thread 对话
# run 完成（CodexSDK 标记 = 路由证据）。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="cx-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W26 Codex 官方 SDK (API=$API RUN_ID=$RUN_ID) =="
nohup python3 adapters/codex/harness.py 8020 > /tmp/codex-sdk.log 2>&1 &
CP=$!
sleep 3
curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
  -d '{"name":"codex-sdk","endpoint":"http://host.docker.internal:8020","version":"v1","capabilities":["gpt-6.1-sol"]}' > /dev/null
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"codex","config":{"model":"gpt-6.1-sol","instructions":"你是简洁的助手。","tools":[],"harness_ref":{"name":"codex-sdk"},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"用一句话说明什么叫幂等"}' -o /tmp/cx-run.out 2>&1 || true
assert "真实 Codex SDK 对话完成（final 含 CodexSDK 标记）" \
  python3 -c 'import json; d=json.load(open("/tmp/cx-run.out")); assert d["status"]=="completed" and "CodexSDK" in d.get("final",""), d' <<< '{}'
assert "账本行落账（Codex 调用留痕）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM llm_calls WHERE run_id=(SELECT id FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1)\" | grep -q 1"

kill $CP 2>/dev/null || true
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
