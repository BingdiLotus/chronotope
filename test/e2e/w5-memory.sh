#!/usr/bin/env bash
# W5 验收 e2e：分层记忆——多轮对话触发消化（阈值经 CONSOLIDATE_THRESHOLD 调小），
# 摘要/条目落库（memory 端点可查）、memory.consolidated 事件、下轮组装注入记忆。
# 前置：worker 以 CONSOLIDATE_THRESHOLD=4 运行；api 运行中。
# 用法: bash test/e2e/w5-memory.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="mem-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W5 分层记忆 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"mem-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是记忆演示助手。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/mem-sse.out 2>&1 &
SSE_PID=$!
sleep 1

# 2 轮对话（每轮 1 user + 1 assistant = 2 条消息；阈值 4 → 第 2 轮结束触发消化）
submit() { # $1=input $2=key；session 限流 429 时重试（1 run/5s）
  for _ in $(seq 1 30); do
    if curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
      -H "Idempotency-Key: $2" -d "{\"input\":\"$1\",\"topic\":\"登录模块\"}" > /dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}
submit "我在做登录模块" "$RUN_ID-1"
submit "用户希望支持短信验证码" "$RUN_ID-2"
# 消化在 run 结束同步触发（journaled）→ 落库立即可查；SSE 保持订阅以捕获后续事件
MEM=$(curl -fsS "$API/sessions/$SID/memory")
assert "摘要落库（summaries ≥1）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert len(d["summaries"])>=1 and d["summaries"][0]["summary"]' <<< "$MEM"
assert "条目抽取（user 消息 → long_term，topic=登录模块）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert any(it["content"]=="我在做登录模块" for it in d["items"]) and all(it["topic"]=="登录模块" for it in d["items"])' <<< "$MEM"

# 组装注入验证：消化后下一轮 run 的 llm.call 事件 msgs 数增长
# （3 条 system：指令 + 主题摘要 + 检索片段 + 4 条历史 + 1 条输入 = 8）。
submit "再补充：密码找回" "$RUN_ID-3"
sleep 1
kill "$SSE_PID" 2>/dev/null || true
assert "memory.consolidated 事件" grep -q '"type":"memory.consolidated"' /tmp/mem-sse.out
assert "下一轮组装注入记忆（llm.call msgs=8，含摘要+检索片段）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in sys.stdin if l.startswith("data: ")]; calls=[e for e in data if e["type"]=="llm.call"]; assert calls and calls[-1]["payload"]["msgs"]==8, calls' < /tmp/mem-sse.out

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
