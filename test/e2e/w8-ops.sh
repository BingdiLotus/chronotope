#!/usr/bin/env bash
# W8 验收 e2e：存储分层与运营（① 会话导出 tar；② 孤儿沙箱 GC）。
# 前置：api/worker/harness/executor 运行中（executor 带 EXECUTOR_GC_INTERVAL=2s）；
#       harness fake 脚本模式（bash 工具产出）。
# 用法: bash test/e2e/w8-ops.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="w8-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W8 存储分层与运营 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

# --- ① 会话导出 tar ---
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"export-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是导出演示助手。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"导出演示对话"}' > /dev/null

rm -rf /tmp/w8-export && mkdir -p /tmp/w8-export
curl -fsS "$API/sessions/$SID/export" -o /tmp/w8-export/session.tar.gz
tar -xzf /tmp/w8-export/session.tar.gz -C /tmp/w8-export
assert "tar 含 5 个标准条目（manifest/events/messages/memory/usage）" \
  python3 -c 'import os; files=set(os.listdir("/tmp/w8-export")); want={"manifest.json","events.ndjson","messages.ndjson","memory.json","usage.csv"}; assert want <= files, files' <<< '{}'
assert "manifest 计数正确（events≥3、messages≥2）" \
  python3 -c 'import json; d=json.load(open("/tmp/w8-export/manifest.json")); c=d["counts"]; assert c["events"]>=3 and c["messages"]>=2, c' <<< '{}'
assert "events.ndjson 含 run.completed（真相随导出）" grep -q 'run.completed' /tmp/w8-export/events.ndjson

# --- ② 孤儿沙箱 GC ---
GAGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"gc-agent","config":{"model":"claude-sonnet-4-6","instructions":"GC 演示助手。","tools":["bash"],"environment":{"sandbox":{"image":"python:3.11-slim","limits":{"cpu":"1","mem":"256m"},"ttl":"4s"}},"version":1}}')
GAID=$(echo "$GAGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
GSID=$(curl -fsS -X POST "$API/agents/$GAID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$GSID/events?after=0" > /tmp/w8-gc-sse.out 2>&1 &
GC_SSE=$!
sleep 1
curl -fsS -m 120 -X POST "$API/sessions/$GSID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-gc" -d '{"input":"创建一个沙箱"}' > /dev/null
sleep 1
kill "$GC_SSE" 2>/dev/null || true
SB_ID=$(grep -a -o '"sandbox_id":"sb_[a-f0-9]*"' /tmp/w8-gc-sse.out | head -1 | cut -d'"' -f4)
echo "  本 run 沙箱: ${SB_ID}"
sleep 8 # ttl 4s + GC 周期 2s → 沙箱应被 GC
assert "过期沙箱被 GC 清理（容器按 id 销毁）" \
  test -z "$(docker ps -aq --filter "name=$SB_ID" 2>/dev/null)"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
