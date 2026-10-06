#!/usr/bin/env bash
# W8 后置 e2e：outbox 事件投递（webhook 通道，Slack/Feishu incoming webhook 形态）
# 订阅 → run 事件同事务入队 → 投递 worker POST 到接收器 → 断言收到事件。
# 前置：harness fake 模式；api（OUTBOX_INTERVAL 已调小）/worker 运行中；
#       接收器夹具已起（test/fixtures/notify-sink.py，默认 9300）。
# 用法: bash test/e2e/w8-notify.sh [API_URL] [SINK_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
SINK="${2:-http://localhost:9300}" # 本地 api 用 localhost；compose 栈（容器内）显式传 host.docker.internal
RUN_ID="ntf-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W8 后置 e2e：outbox 事件投递（API=${API}，SINK=${SINK}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"ntf","config":{"model":"claude-sonnet-4-6","instructions":"i","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

assert "订阅 webhook（SSRF 放行本地接收器）" \
  curl -fsS -X POST "$API/sessions/$SID/subscriptions" -H 'content-type: application/json' \
  -d "{\"channel\":\"webhook\",\"target\":\"$SINK\"}"

curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"触发事件投递"}' > /dev/null
sleep 6 # 投递周期（OUTBOX_INTERVAL）

assert "投递方收到 run.started 事件" \
  bash -c 'grep -q "run.started" /tmp/notify-sink.ndjson'
assert "投递方收到 run.completed 事件（终态事件随包）" \
  bash -c 'grep -q "run.completed" /tmp/notify-sink.ndjson'
assert "投递载荷含会话归属与时间戳" \
  python3 test/fixtures/check-sink.py "$SID"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
