#!/usr/bin/env bash
# 多租户身份 e2e（API_AUTH_MODE=on）：无 key 401 / 错 key 401 / org 隔离 403 /
# 正确 key 全链路 / key 明文一次性。匿名路径（healthz/webhooks）放行。
# 前置：api 以 API_AUTH_MODE=on 运行。
# 用法: bash test/e2e/auth.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="auth-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 多租户身份 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

assert "无 key 请求 401" \
  bash -c 'curl -sS -o /dev/null -w "%{http_code}" '"$API"'/orgs/x/sessions | grep -q 401'
assert "错 key 请求 401" \
  bash -c 'curl -sS -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ck_wrong" '"$API"'/orgs/x/sessions | grep -q 401'

# 管理面：admin key 引导（全放行）
ADMIN="${ADMIN_KEY:-ck_admin_bootstrap}"
AUTH_ADMIN="Authorization: Bearer $ADMIN"

# org A：admin 建 agent → 生成 A 的 key → 全程用 A key
ORG_A="org-$RUN_ID-a"
curl -fsS -X POST "$API/orgs/$ORG_A/agents" -H "$AUTH_ADMIN" -H 'content-type: application/json' \
  -d '{"name":"a","config":{"model":"claude-sonnet-4-6","instructions":"i","tools":[],"version":1}}' > /dev/null
KEY_A=$(curl -fsS -X POST "$API/orgs/$ORG_A/keys" -H "$AUTH_ADMIN" | python3 -c 'import sys,json;print(json.load(sys.stdin)["key"])')
assert "key 明文格式（ck_ 前缀 ≥32 字符）" \
  bash -c 'echo "$1" | grep -qE "^ck_[a-f0-9]{64}$"' _ "$KEY_A"

AUTH_A="Authorization: Bearer $KEY_A"
assert "正确 key 访问本 org 会话列表 200" \
  bash -c 'curl -sS -o /dev/null -w "%{http_code}" -H "$1" '"$API"'/orgs/'"$ORG_A"'/sessions | grep -q 200' _ "$AUTH_A"

# org B：key 隔离
ORG_B="org-$RUN_ID-b"
curl -fsS -X POST "$API/orgs/$ORG_B/agents" -H "$AUTH_ADMIN" -H 'content-type: application/json' \
  -d '{"name":"b","config":{"model":"claude-sonnet-4-6","instructions":"i","tools":[],"version":1}}' > /dev/null
assert "A 的 key 访问 B 的 org → 403（租户隔离）" \
  bash -c 'curl -sS -o /dev/null -w "%{http_code}" -H "$1" '"$API"'/orgs/'"$ORG_B"'/sessions | grep -q 403' _ "$AUTH_A"
assert "匿名路径（healthz）不受 on 模式限制" \
  bash -c 'curl -sS -o /dev/null -w "%{http_code}" '"$API"'/healthz | grep -q 200'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
