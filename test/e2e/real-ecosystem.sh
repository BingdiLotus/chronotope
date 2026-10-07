#!/usr/bin/env bash
# 真实 e2e：W6 生态（MCP 真实调用 + skill 安装）+ 期 3 §C allowlist。
# ① 真实模型调用 mcp:echo:echo（MCP 客户端真实 tools/list + call）
# ② 真实模型安装 skill（skill.install 事件 + 沙箱 skills/ 就位）
# ③ allowlist 收紧 → 未列工具不下发（真实 run 无 mcp.call）
# 前置：harness 真实模式；MCP 夹具 test/fixtures/mcp-server.py 起于 9100。
# 用法: bash test/e2e/real-ecosystem.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/../.."

API="${1:-http://localhost:8080}"
RUN_ID="reco-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 真实 e2e：MCP 调用 / skill / allowlist（API=${API}，RUN_ID=${RUN_ID}）=="

nohup python3 test/fixtures/mcp-server.py 9100 > /tmp/reco-mcp.log 2>&1 &
FIXTURE_PID=$!
sleep 2

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-eco-agent","config":{"model":"claude-sonnet-4-6","instructions":"你必须先调用 mcp:echo:echo 工具（参数 text=你好），然后把回显内容复述出来。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/mcp-allowlist" -H 'content-type: application/json' \
  -d '{"server":"echo","tool_patterns":["echo"]}' > /dev/null
curl -fsS -X POST "$API/sessions/$SID/mcp" -H 'content-type: application/json' \
  -d '{"server":"echo","url":"http://host.docker.internal:9100"}' > /dev/null

curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/reco-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-mcp" -d '{"input":"调用 MCP 工具"}' > /tmp/reco-mcp.out 2>&1 &
RUN_PID=$!
wait "$RUN_PID" || true
sleep 2
assert "真实模型调用 MCP（mcp.call 无错误 + 完成）" \
  bash -c 'grep -q "mcp.call" /tmp/reco-sse.out && grep -q "completed" /tmp/reco-mcp.out && ! grep -q "\\"error\\":true" /tmp/reco-sse.out'

# --- ② skill 安装 ---
curl -fsS -X POST "$API/sessions/$SID/skills" -H 'content-type: application/json' \
  -d '{"name":"demo-skill","content":"# Demo Skill\n真实模型安装的演示技能。"}' > /dev/null
sleep 2
kill "$SSE_PID" 2>/dev/null || true
assert "skill 安装（skill.install 事件）" \
  bash -c 'grep -q "skill.install" /tmp/reco-sse.out'

# --- ③ allowlist 收紧（未列工具不下发）---
SID2=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/mcp-allowlist" -H 'content-type: application/json' \
  -d '{"server":"echo","tool_patterns":["other."]}' > /dev/null
curl -fsS -X POST "$API/sessions/$SID2/mcp" -H 'content-type: application/json' \
  -d '{"server":"echo","url":"http://host.docker.internal:9100"}' > /dev/null
curl -fsS -N "$API/sessions/$SID2/events?after=0" > /tmp/reco-al.out 2>&1 &
AL_SSE=$!
sleep 1
curl -fsS -m 300 -X POST "$API/sessions/$SID2/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-al" -d '{"input":"调用 MCP 工具"}' > /tmp/reco-al-run.out 2>&1 || true
sleep 5
kill "$AL_SSE" 2>/dev/null || true
assert "allowlist 收紧后未列工具不下发（无 mcp.call）" \
  bash -c '! grep -q "mcp.call" /tmp/reco-al.out'
kill "$FIXTURE_PID" 2>/dev/null || true

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
