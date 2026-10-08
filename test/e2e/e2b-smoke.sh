#!/usr/bin/env bash
# E2B driver 真实实例 smoke（env 门控，非 CI 常规步骤——与 real-model.sh 同策略）。
# 前置：E2B_API_KEY（+ 可选 E2B_API_URL 自托管 / E2B_TEMPLATE）；
#        executor 以 EXECUTOR_DRIVER=e2b 运行；api/worker/harness 运行中。
#
# 官方云诊断（2026-10 实证）：官方云平台 API 只有沙箱生命周期 REST
# （创建/列表/暂停/恢复/删除）；命令执行与文件走 envd 的 ConnectRPC
# （protobuf 流式——POST /v2/sandboxes/{id}/commands 不存在：
#  "no matching operation was found"）。HTTPE2BAPI 的 /commands 协议是
# 自托管 E2B 网关的形状——官方云命令执行需 ConnectRPC 对接（后置子任务）。
# 场景：W2 沙箱闭环（bash 执行 + 文件写入/读取）在 E2B 沙箱上全链路跑通。
# 模型模式：fake 脚本（真实模型非确定性——127 实证；E2B 指标基准需确定性；
# 真实模型全链路在 real-e2e.sh 覆盖）。
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

# fake 脚本 harness（bash + write_file + read_file 确定性工具链）——compose
# 重建（demo.sh 同法：worker 容器经 harness 服务名连，本地进程不在容器网络）
DC="docker compose --env-file .env -f deploy/docker-compose.yml"
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"echo e2b-ok"}}},{"tool_call":{"name":"write_file","arguments":{"path":"/tmp/e2b.txt","content":"from-e2b"}}},{"tool_call":{"name":"read_file","arguments":{"path":"/tmp/e2b.txt"}}},{"final":"E2B 沙箱工具链完成。"}]' $DC up -d --force-recreate harness > /dev/null 2>&1
sleep 5
curl -fsS localhost:8000/healthz > /dev/null && echo "harness fake 脚本就绪"

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
