#!/usr/bin/env bash
# E2B driver 真实实例 smoke（env 门控，非 CI 常规步骤——与 real-model.sh 同策略）。
# 前置：E2B_API_KEY（+ 可选 E2B_API_URL 自托管 / E2B_TEMPLATE）；
#        executor 以 EXECUTOR_DRIVER=e2b 运行；api/worker/harness 运行中。
# 场景：W2 沙箱闭环（bash 执行 + 文件写入/读取）在 E2B 沙箱上全链路跑通。
# 用法: E2B_API_KEY=... E2B_TEMPLATE=... bash test/e2e/e2b-smoke.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
if [ -z "${E2B_API_KEY:-}" ]; then
  echo "跳过：未设置 E2B_API_KEY（真实实例 smoke 需凭证；自托管另设 E2B_API_URL）"
  exit 0
fi

RUN_ID="e2b-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== E2B driver 真实实例 smoke（API=${API}，RUN_ID=${RUN_ID}，E2B_API_URL=${E2B_API_URL:-https://api.e2b.dev}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"e2b","config":{"model":"claude-sonnet-4-6","instructions":"i","tools":["bash","write_file","read_file"],"environment":{"sandbox":{"image":"base","limits":{"cpu":"2","mem":"512m"}}},"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"在沙箱里执行 echo 并写一个文件"}' > /tmp/e2b-run.out 2>&1 &
RUN_PID=$!
wait "$RUN_PID" || true

assert "E2B 沙箱 run 完成" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/e2b-run.out
curl -fsS -N -m 3 "$API/sessions/$SID/events?after=0" > /tmp/e2b-sse.out 2>&1 || true
assert "sandbox.exec 事件（E2B 沙箱执行成功）" \
  grep -q '"type":"sandbox.exec"' /tmp/e2b-sse.out

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
