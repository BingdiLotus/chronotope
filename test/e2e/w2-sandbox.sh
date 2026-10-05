#!/usr/bin/env bash
# W2 验收 e2e：agent 写代码 → 沙箱执行 → 文件回传（真实 docker 沙箱）。
# 前置：api/worker/executor 运行中（make dev-up + register-worker）；
#       harness 以 HARNESS_FAKE_MODEL=1 + HARNESS_FAKE_SCRIPT 运行（脚本轮次循环，
#       见 harness/README；多次运行无需重启）：
#   HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/hello.py","content":"print(42)"}}},
#   {"tool_call":{"name":"bash","arguments":{"command":"python3 /workspace/hello.py"}}},
#   {"tool_call":{"name":"read_file","arguments":{"path":"/workspace/hello.py"}}},
#   {"final":"沙箱闭环完成，输出 42。"}]'
# 用法: bash test/e2e/w2-sandbox.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="w2-$(date +%s)"
PASS=0; FAIL=0

pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W2 沙箱闭环 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

# 1. 建 agent（代码工具 + 沙箱规格）
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' -d '{
  "name":"w2-coder",
  "config":{
    "model":"claude-sonnet-4-6","instructions":"你是沙箱演示助手。",
    "tools":["bash","write_file","read_file"],
    "environment":{"sandbox":{"image":"python:3.12-slim","limits":{"cpu":"1","mem":"256m"},"ttl":"1h"}},
    "version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
assert "创建 agent（工具 + 沙箱规格）" python3 -c 'import sys,json; d=json.load(sys.stdin)["config"]; assert "bash" in d["tools"] and d["environment"]["sandbox"]["image"]' <<< "$AGENT"

# 2. 建 session + SSE 订阅
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/w2-sse.out 2>&1 &
SSE_PID=$!
sleep 1

# 3. 提交任务：写代码 → 沙箱跑 → 读回
RUN=$(curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"写一个 hello.py 并运行"}')
assert "任务完成（final 非空）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["final"]' <<< "$RUN"
assert "4 个 step（write_file→bash→read_file→final）" \
  python3 -c 'import sys,json; assert json.load(sys.stdin)["steps"]==4' <<< "$RUN"

sleep 2
kill "$SSE_PID" 2>/dev/null || true

# 4. 事件断言：3 次 tool.call + 3 次 sandbox.exec，全 exit 0
assert "tool.call ×3" test "$(grep -c '"type":"tool.call"' /tmp/w2-sse.out || true)" -ge 3
assert "sandbox.exec ×3（write_file/bash/read_file）" \
  test "$(grep -c '"type":"sandbox.exec"' /tmp/w2-sse.out || true)" -ge 3
assert "sandbox.exec 全部 exit=0" \
  python3 -c 'import sys,json; data=[json.loads(l[6:])["payload"] for l in sys.stdin if l.startswith("data: ") and "sandbox.exec" in l]; assert data and all(p["exit"]==0 for p in data), data' < /tmp/w2-sse.out
assert "bash 输出 42" \
  python3 -c 'import sys,json; data=[json.loads(l[6:])["payload"] for l in sys.stdin if l.startswith("data: ") and "sandbox.exec" in l]; assert any(p.get("tool")=="bash" and "42" in p.get("output","") for p in data)' < /tmp/w2-sse.out
assert "read_file 读回 hello.py" \
  python3 -c 'import sys,json; data=[json.loads(l[6:])["payload"] for l in sys.stdin if l.startswith("data: ") and "sandbox.exec" in l]; assert any(p.get("tool")=="read_file" and p.get("path")=="/workspace/hello.py" for p in data)' < /tmp/w2-sse.out

# 5. 事件顺序：run.started → …工具循环… → run.completed
ORDER=$(grep -o '"type":"run\.[a-z]*"' /tmp/w2-sse.out | paste -sd, -)
assert "首事件 run.started、末事件 run.completed" \
  python3 -c "import sys; s=sys.stdin.read().strip().split(','); assert 'run.started' in s[0] and 'run.completed' in s[-1], s" <<< "$ORDER"
echo "  事件序列: $ORDER"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
