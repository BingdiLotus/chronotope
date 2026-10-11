#!/usr/bin/env bash
# Harness 装配阶段 4（第三方接入验证）：最小第三方 harness（纯 stdlib）经
# /runs 契约接入——conformance 帧断言 + 注册表注册 + agent 绑定 → run 走
# 第三方（final 标记 = 路由证据）。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="tp-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W24 第三方 harness 接入 (API=$API RUN_ID=$RUN_ID) =="
# ① 起第三方 harness + 帧级 conformance（直打 /runs 验证帧形状）
nohup python3 test/fixtures/third-party-harness.py 9011 > /tmp/tp-harness.log 2>&1 &
TP_PID=$!
sleep 2
curl -fsS -m 10 -X POST localhost:9011/runs -H 'content-type: application/json' \
  -d '{"protocol":"1.0","run_id":"tp-conformance","session_id":"s1","step":0,"model":"m","messages":[{"role":"user","content":"hi","source":"trusted"}],"tools":[],"context":{}}' \
  -o /tmp/tp-frames.out 2>&1
assert "第三方帧 conformance（beat/delta/done——done 唯一终态）" \
  python3 -c 'import sys
t=open("/tmp/tp-frames.out").read()
assert "beat" in t and "delta" in t, "帧缺 beat/delta"
assert t.count("done") == 1, "done 非唯一终态"' <<< '{}'

# ② 注册表注册 + agent 绑定
curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
  -d '{"name":"thirdparty","endpoint":"http://host.docker.internal:9011","version":"v1","capabilities":["claude-sonnet-4-6"]}' > /dev/null
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"tp","config":{"model":"claude-sonnet-4-6","instructions":"i","tools":[],"harness_ref":{"name":"thirdparty"},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ③ run 走第三方（final 含第三方标记 = 路由证据）
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"hi"}' -o /tmp/tp-run.out 2>&1 || true
assert "run 完成且 final 含第三方标记（0 平台改动换 harness）" \
  python3 -c 'import json; d=json.load(open("/tmp/tp-run.out")); assert d["status"]=="completed" and "第三方回复" in d.get("final",""), d' <<< '{}'

kill $TP_PID 2>/dev/null || true
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
