#!/usr/bin/env bash
# 批 1 Claude 官方 SDK 接入 e2e：注册表注册 + agent 绑定 → 真实 SDK 对话
# run 完成（ClaudeSDK 标记）+ 账本 token 非零。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="cl-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W25 Claude 官方 SDK (API=$API RUN_ID=$RUN_ID) =="
# ① 起官方 SDK 适配器 + 注册表 + 绑定
nohup python3 adapters/claude/harness.py 8010 > /tmp/claude-sdk.log 2>&1 &
CP=$!
sleep 3
curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
  -d '{"name":"claude-sdk","endpoint":"http://host.docker.internal:8010","version":"v1","capabilities":["claude-sonnet-4-6"]}' > /dev/null
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"claude","config":{"model":"claude-sonnet-4-6","instructions":"你是简洁的助手。","tools":[],"harness_ref":{"name":"claude-sdk"},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ② 真实 SDK 对话 run（ClaudeSDK 标记 = 路由证据）
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"用一句话说明什么叫幂等"}' -o /tmp/cl-run.out 2>&1 || true
assert "真实 Claude SDK 对话完成（final 含 ClaudeSDK 标记）" \
  python3 -c 'import json; d=json.load(open("/tmp/cl-run.out")); assert d["status"]=="completed" and "ClaudeSDK" in d.get("final",""), d' <<< '{}'
assert "账本 token 非零（SDK 真实 usage 落账）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM llm_calls WHERE run_id=(SELECT id FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1) AND tokens_in > 0\" | grep -q 1"

kill $CP 2>/dev/null || true
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
