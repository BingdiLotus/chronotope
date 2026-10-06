#!/usr/bin/env bash
# W6 验收 e2e：子 Agent（spawn_subagent）——父 agent 派发子任务 → 子会话/子 run
# 独立执行（durable 等待）→ subagent.spawned/completed 事件 → 结果回喂父 agent。
# 前置：api/worker/harness 运行中；harness fake 脚本模式（父 agent 脚本产出
# spawn_subagent 工具调用；子 agent 用 model=chronotope-subagent 确定性终答防递归）。
# 用法: bash test/e2e/w6-subagent.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="sub-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W6 子 Agent e2e（API=${API}，RUN_ID=${RUN_ID}）=="

# 子 agent：由编排方创建并经环境注入（harness 脚本引用其 id，脚本为静态 JSON）
CHILD_ID="${CHILD_AGENT_ID:-}"
if [ -z "$CHILD_ID" ]; then
  echo "  需 CHILD_AGENT_ID 环境变量（先创建子 agent 并让 harness 脚本引用其 id）"
  exit 2
fi
# 父 agent：可派发子任务
PARENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"parent-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是父协调者。","tools":["spawn_subagent"],"version":1}}')
PARENT_ID=$(echo "$PARENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$PARENT_ID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/sub-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"派发子任务计算 2+2"}' > /tmp/sub-run.out 2>&1 &
RUN_PID=$!
wait "$RUN_PID" || true
# 等 subagent.completed 送达（事件经 poller 流式，与提交响应存在竞态）
for i in $(seq 1 15); do
  grep -q '"type":"subagent.completed"' /tmp/sub-sse.out 2>/dev/null && break
  sleep 1
done
kill "$SSE_PID" 2>/dev/null || true

assert "父 run 完成" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["steps"]>=2' < /tmp/sub-run.out
assert "subagent.spawned 事件（含 child_run_id）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/sub-sse.out") if l.startswith("data: ")]; ev=[e for e in data if e["type"]=="subagent.spawned"]; assert ev and ev[0]["payload"]["child_run_id"]' <<< '{}'
assert "subagent.completed 事件（含子任务 final）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/sub-sse.out") if l.startswith("data: ")]; ev=[e for e in data if e["type"]=="subagent.completed"]; assert ev and "42" in str(ev[0]["payload"])' <<< '{}'

# 子会话独立时间轴：run.started → llm.call → run.completed
CHILD_SID=$(python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/sub-sse.out") if l.startswith("data: ")]; ev=[e for e in data if e["type"]=="subagent.spawned"]; print(ev[0]["payload"]["child_session_id"])')
curl -fsS -N -m 5 "$API/sessions/$CHILD_SID/events?after=0" > /tmp/sub-child-sse.out 2>&1 || true
assert "子会话时间轴完整（started→llm.call→completed）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/sub-child-sse.out") if l.startswith("data: ")]; types=[e["type"] for e in data]; assert types[0]=="run.started" and "llm.call" in types and "run.completed" in types, types' <<< '{}'
assert "子任务最终答案=42（fake 子代理终答）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/sub-child-sse.out") if l.startswith("data: ")]; ev=[e for e in data if e["type"]=="run.completed"]; assert ev and "42" in ev[0]["payload"]["final"], ev' <<< '{}'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
