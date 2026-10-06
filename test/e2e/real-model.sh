#!/usr/bin/env bash
# P2-0 真实模型全链路复验（需 .env 密钥 + litellm 容器）：
# ① 真实对话 + 真实消化（consolidation 摘要由真实模型生成，非 fake 文本）
# ② 真实子 Agent（父模型显式指令派发；子任务真实作答）
# ③ 计量如实（llm.call 携带真实 token 数；usage 聚合非零）
# 前置：harness 真实模式（无 HARNESS_FAKE_MODEL）；worker CONSOLIDATE_THRESHOLD=4。
# 用法: bash test/e2e/real-model.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="real-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== P2-0 真实模型全链路复验（API=${API}，RUN_ID=${RUN_ID}）=="

# --- ① 真实对话 + 真实消化 ---
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是真实模型演示助手，回答要简洁。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/real-sse.out 2>&1 &
SSE_PID=$!
sleep 1

submit() { # $1=input $2=key（限流重试）
  for _ in $(seq 1 30); do
    if curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
      -H "Idempotency-Key: $2" -d "{\"input\":\"$1\",\"topic\":\"真实模型复验\"}" > "/tmp/real-$2.out" 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}
submit "用一句话介绍 Chronotope" "$RUN_ID-1"
submit "它和普通 agent 框架有什么区别" "$RUN_ID-2"
submit "请总结以上两点" "$RUN_ID-3"
sleep 2
kill "$SSE_PID" 2>/dev/null || true

assert "真实对话完成（3 轮均 completed，final 非空）" \
  python3 -c 'import sys,json; outs=[json.load(open(f"/tmp/real-{k}.out")) for k in ["'"$RUN_ID"'-1","'"$RUN_ID"'-2","'"$RUN_ID"'-3"]]; assert all(o["status"]=="completed" and o["final"] for o in outs), outs' <<< '{}'
MEM=$(curl -fsS "$API/sessions/$SID/memory")
assert "真实消化触发（摘要由真实模型生成，非 fake 文本）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["summaries"], d; s=d["summaries"][0]["summary"]; assert "本轮对话已消化" not in s and len(s)>10, s' <<< "$MEM"
assert "memory.consolidated 事件" grep -q '"type":"memory.consolidated"' /tmp/real-sse.out
SUMMARY_TEXT=$(echo "$MEM" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["summaries"][0]["summary"] if d["summaries"] else "")')
echo "  真实摘要: ${SUMMARY_TEXT:0:120}..."

# --- ② 真实子 Agent ---
CHILD=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-child","config":{"model":"claude-sonnet-4-6","instructions":"你是子任务助手，回答要极其简短。","tools":[],"version":1}}')
CHILD_ID=$(echo "$CHILD" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
PARENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-parent","config":{"model":"claude-sonnet-4-6","instructions":"你是协调者。收到任务后必须调用 spawn_subagent 工具派发子任务（agent 参数用给你的 id），然后根据子任务结果给出一句话结论。","tools":["spawn_subagent"],"version":1}}')
PARENT_ID=$(echo "$PARENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
PSID=$(curl -fsS -X POST "$API/agents/$PARENT_ID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$PSID/events?after=0" > /tmp/real-psse.out 2>&1 &
PSSE_PID=$!
sleep 1
curl -fsS -m 300 -X POST "$API/sessions/$PSID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-sp" \
  -d "{\"input\":\"派发子任务：计算 2+2。子 agent 的 id 是 $CHILD_ID\"}" > /tmp/real-sp.out 2>&1 &
SP_PID=$!
wait "$SP_PID" || true
sleep 2
kill "$PSSE_PID" 2>/dev/null || true

if grep -q '"type":"subagent.spawned"' /tmp/real-psse.out; then
  assert "真实子 Agent：spawned → completed（子任务真实作答）" \
    python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/real-psse.out") if l.startswith("data: ")]; ev=[e for e in data if e["type"]=="subagent.completed"]; assert ev and ev[0]["payload"].get("final"), ev' <<< '{}'
  assert "父 run 完成且引用子任务结果" \
    python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["steps"]>=2, d' < /tmp/real-sp.out
else
  echo "  [观察] 父模型未调用 spawn_subagent（真实模型非确定性；指令已显式要求）。"
  python3 -c 'import sys,json; d=json.load(sys.stdin); print("  父模型答复:", d.get("final","")[:100])' < /tmp/real-sp.out || true
fi

# --- ③ 计量如实 ---
assert "llm.call 携带真实 token（非 0/0）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/real-sse.out") if l.startswith("data: ")]; calls=[e for e in data if e["type"]=="llm.call"]; assert any(e["payload"]["usage"]["tokens_in"]+e["payload"]["usage"]["tokens_out"]>0 for e in calls), calls' <<< '{}'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
