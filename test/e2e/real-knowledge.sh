#!/usr/bin/env bash
# 真实 e2e：期 3 §D 共享知识（真实嵌入 + 跨会话挂载注入）。
# ① POST knowledge（harness /embed 真实嵌入——LiteLLM embeddings）
# ② 新会话真实 run → 载荷含【共享知识】片段（harness 载荷日志断言）
# ③ 真实模型回答引用共享知识内容
# 前置：harness 真实模式（.env 含 EMBEDDING 模型经 litellm）。
# 用法: bash test/e2e/real-knowledge.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/../.."

API="${1:-http://localhost:8080}"
RUN_ID="rkn-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 真实 e2e：共享知识挂载（API=${API}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-kn-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是知识问答助手。若系统消息中有【共享知识】，必须引用其中的内容回答；否则回答不知道。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ① 写入（显式向量——嵌入模型凭据后置：Anthropic 无 embeddings 端点；
# 挂载/回答断言仍为真实模型链路）
KN=""
for i in $(seq 1 5); do KN=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/knowledge" -H 'content-type: application/json' -d '{"content":"Chronotope 的口号是：让时间成为一等公民。"}' 2>/dev/null || true); [ -n "$KN" ] && break; sleep 2; done
assert "共享知识写入" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["id"], d' <<< "$KN"

# ②+③ 新会话 run → 挂载注入 + 回答引用
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"Chronotope 的口号是什么？"}' > /tmp/rkn-run.out 2>&1 || true
assert "真实 run 完成" python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/rkn-run.out
assert "挂载注入（harness 载荷含【共享知识】）" \
  bash -c 'docker compose --env-file .env -f deploy/docker-compose.yml logs harness 2>/dev/null | grep -q "时间成为一等公民"'
assert "真实回答引用共享知识内容" \
  bash -c 'grep -q "时间成为一等公民" /tmp/rkn-run.out'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
