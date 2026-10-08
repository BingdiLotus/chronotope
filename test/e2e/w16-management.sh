#!/usr/bin/env bash
# 期 5 §A：多租户管理面 e2e——成员/用量聚合/配额/账单导出的完整链路。
# 前置：docker compose 全栈运行中（fake harness）。
# 用法: bash test/e2e/w16-management.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="/opt/homebrew/bin:$PATH"
API="${1:-http://localhost:8080}"
RUN_ID="w16-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 多租户管理面 e2e（期 5 §A，API=${API}，RUN_ID=${RUN_ID}）=="

# 前置：计量聚合短周期（默认 1m——用量断言需先落库，usage 0 行实证）
API_AGGREGATE_INTERVAL=5s docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate api > /dev/null 2>&1
sleep 5
for i in $(seq 1 20); do curl -fsS localhost:8080/healthz > /dev/null 2>&1 && break; sleep 1; done

# 1. 成员管理：建 org → 加成员（角色）→ 列表 → 移除
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"mgmt-agent","config":{"model":"claude-sonnet-4-6","instructions":"一句话。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
# 建 user（principal 化——015 的 users）
USER_RESP=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/users" -H 'content-type: application/json' \
  -d '{"name":"管理员"}' 2>/dev/null || echo '{}')
UID1=$(echo "$USER_RESP" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("id",""))' 2>/dev/null || echo "")
[ -n "$UID1" ] && pass "技术主体创建（principal）" || fail "用户创建失败"
curl -fsS -X POST "$API/orgs/org-$RUN_ID/members" -H 'content-type: application/json' \
  -d "{\"user_id\":\"$UID1\",\"role\":\"org_admin\"}" > /tmp/w16-add.out 2>&1
assert "成员添加（org_admin）" grep -q 'org_admin' /tmp/w16-add.out
assert "成员列表" bash -c "curl -fsS '$API/orgs/org-$RUN_ID/members' | grep -q '$UID1'"

# 2. 用量：跑一个会话 → 聚合查询（day 粒度）
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"hello"}' > /dev/null 2>&1 || true
for i in $(seq 1 30); do
  curl -fsS "$API/orgs/org-$RUN_ID/usage?granularity=day" 2>/dev/null | grep -qE '\"active_seconds\"' && break
  sleep 2
done
curl -fsS "$API/orgs/org-$RUN_ID/usage?granularity=day" > /tmp/w16-usage.out 2>&1
assert "用量聚合（granularity=day 返回 usage 数组）" \
  bash -c "python3 -c 'import json;d=json.load(open(\"/tmp/w16-usage.out\"));assert \"usage\" in d and isinstance(d[\"usage\"],list)'"

# 3. 配额读改
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/quota" -H 'content-type: application/json' \
  -d '{"daily_token_budget":5000,"daily_compute_seconds":600}' > /tmp/w16-quota.out 2>&1
assert "配额写入并回读（daily_token_budget=5000）" \
  bash -c "python3 -c 'import json;d=json.load(open(\"/tmp/w16-quota.out\"));q=d.get(\"quotas\",{});assert q.get(\"daily_token_budget\")==5000,q'"

# 4. 账单导出（CSV）
curl -fsS "$API/orgs/org-$RUN_ID/billing/export" -o /tmp/w16-billing.csv 2>&1
assert "账单 CSV 导出（头部 + 数据行）" \
  bash -c 'head -1 /tmp/w16-billing.csv | grep -q "bucket,active_seconds" && [ $(wc -l < /tmp/w16-billing.csv) -ge 2 ]' 

rm -f /tmp/w16-*.out /tmp/w16-billing.csv
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
