#!/usr/bin/env bash
# W6 生态 e2e：MCP 连接 + skill 安装（落地方案 §11）。
#   MCP：worker 托管客户端（HTTP transport）→ tools/list 缓存 → 模型工具清单
#        注入 mcp:<server>:<tool> → 调用 → mcp.call 事件 + 结果回喂。
#   Skill：沙箱 skills/<name>/SKILL.md + 会话状态 + skill.install 事件。
# 前置：harness fake 脚本模式（moderator 脚本调 mcp:echo:echo）；
#       MCP 夹具已起（test/fixtures/mcp-server.py，默认 9100）。
# 用法: bash test/e2e/w6-mcp.sh [API_URL] [MCP_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
MCP_URL="${2:-http://host.docker.internal:9100}"
RUN_ID="mcp-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W6 生态 e2e（API=${API}，MCP=${MCP_URL}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"mcp-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是 MCP 演示助手。","tools":["bash"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# --- ① MCP 连接 ---
assert "MCP 连接注册（echo server）" \
  curl -fsS -X POST "$API/sessions/$SID/mcp" -H 'content-type: application/json' \
  -d "{\"server\":\"echo\",\"url\":\"$MCP_URL\"}"

# --- ② skill 安装（沙箱懒创建 + SKILL.md 写入）---
assert "skill 安装（沙箱懒创建 + SKILL.md）" \
  curl -fsS -X POST "$API/sessions/$SID/skills" -H 'content-type: application/json' \
  -d '{"name":"data-tools","content":"# 数据工具\ncsv 转换技能：read_file 本文件获取用法。"}'

curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/mcp-sse.out 2>&1 &
SSE_PID=$!
sleep 1

# --- ③ run：模型调 mcp:echo:echo → 结果回喂 → 终答 ---
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"调用 echo 工具说你好"}' > /tmp/mcp-run.out 2>&1 &
RUN_PID=$!
wait "$RUN_PID" || true
sleep 2
kill "$SSE_PID" 2>/dev/null || true

assert "run 完成（MCP 结果回喂 → 终答）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["steps"]>=2, d' < /tmp/mcp-run.out
assert "mcp.call 事件（server/tool 归属）" \
  grep -q '"type":"mcp.call"' /tmp/mcp-sse.out
assert "skill.install 事件" \
  grep -q '"type":"skill.install"' /tmp/mcp-sse.out

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
