#!/usr/bin/env bash
# 批 4 dsh 接入 e2e：适配器（headless --json）+ 注册表 + 绑定 → run 走
# dsh（DshSDK 标记 = 路由证据）+ 账本 usage。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="ds-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W28 dsh 接入 (API=$API RUN_ID=$RUN_ID) =="
# api 就绪等待 + 旧残留清理（W28 在 W26 后——api 忙的 502 与断言读旧
# 残留的假阳性链——与 W25 同款根因）
rm -f /tmp/ds-run.out
for i in $(seq 1 30); do curl -fsS "$API/healthz" > /dev/null 2>&1 && break; sleep 1; done
DSH_RUNTIME_BIN=node DSH_RUNTIME_ARGS="/tmp/dsh/apps/cli/lib/bin.js --profile headless --patch <cordis> --json" \
  nohup node adapters/dsh/harness.mjs 8040 > /tmp/dsh-sdk.log 2>&1 &
DP=$!
sleep 3
curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
  -d '{"name":"dsh-sdk","endpoint":"http://host.docker.internal:8040","version":"v1","capabilities":["gpt-6.1-sol"]}' > /dev/null
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"dsh","config":{"model":"gpt-6.1-sol","instructions":"你是简洁的助手。","tools":[],"harness_ref":{"name":"dsh-sdk"},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"用一句话说明什么叫幂等"}' -o /tmp/ds-run.out 2>&1 || true
assert "dsh 真实 run 完成（final 含 DshSDK 标记——openai 兼容路由证据）" \
  python3 -c 'import json; d=json.load(open("/tmp/ds-run.out")); assert d["status"]=="completed" and "DshSDK" in d.get("final",""), d' <<< '{}'
assert "账本 token 非零（dsh step_end 的 usage 落账）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM llm_calls WHERE run_id=(SELECT id FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1) AND tokens_in > 0\" | grep -q 1"

kill $DP 2>/dev/null || true
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
