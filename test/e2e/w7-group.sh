#!/usr/bin/env bash
# W7 验收 e2e：群聊多 Agent（落地方案 §14 水平黑板拓扑）——
# moderator 主持循环（next_speaker 决策 journaled）→ 成员 child run 各发言
# （group.turn 归属）→ 共享消息日志 → moderator 终答。
# 前置：harness fake 脚本模式（moderator 脚本：两轮 next_speaker + 终答）；
#       成员用 chronotope-participant-a/b 模型（FakeProvider 确定性发言）。
# 用法: bash test/e2e/w7-group.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="grp-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W7 群聊多 Agent e2e（API=${API}，RUN_ID=${RUN_ID}）=="

ORG="org-$RUN_ID"
mk_agent() { # $1=name $2=model
  curl -fsS -X POST "$API/orgs/$ORG/agents" -H 'content-type: application/json' \
    -d "{\"name\":\"$1\",\"config\":{\"model\":\"$2\",\"instructions\":\"群聊演示成员。\",\"tools\":[],\"version\":1}}" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
}
MODERATOR=$(mk_agent moderator claude-sonnet-4-6)
MEMBER_A=$(mk_agent member-a chronotope-participant-a)
MEMBER_B=$(mk_agent member-b chronotope-participant-b)

# 群聊会话：moderator 为主 agent + 两成员
SESS=$(curl -fsS -X POST "$API/agents/$MODERATOR/sessions" -H 'content-type: application/json' \
  -d "{\"participants\":[{\"agent_id\":\"$MEMBER_A\",\"role\":\"架构师\"},{\"agent_id\":\"$MEMBER_B\",\"role\":\"评审\"}]}")
SID=$(echo "$SESS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
assert "群聊会话创建（participants 回显）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert len(d["participants"])==2' <<< "$SESS"

curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/grp-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"讨论：选方案 A 还是 B"}' > /tmp/grp-run.out 2>&1 &
RUN_PID=$!
wait "$RUN_PID" || true
sleep 2
kill "$SSE_PID" 2>/dev/null || true

assert "moderator 终答完成（steps≥3）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["steps"]>=3 and d["final"], d' < /tmp/grp-run.out
assert "group.turn ×2（发言者归属：架构师 + 评审）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/grp-sse.out") if l.startswith("data: ")]; turns=[e for e in data if e["type"]=="group.turn"]; assert len(turns)==2, turns; roles=[t["payload"].get("role") for t in turns]; assert "架构师" in roles and "评审" in roles, roles' <<< '{}'
assert "成员发言回喂（tool 消息含两段发言）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/grp-sse.out") if l.startswith("data: ")]; msgs=[e for e in data if e["type"]=="llm.call"]; assert len(msgs)>=3, msgs' <<< '{}'
assert "两成员子会话独立完成（child run 各 1 次）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/grp-sse.out") if l.startswith("data: ")]; turns=[e for e in data if e["type"]=="group.turn"]; assert all(t["payload"]["child_run_id"] for t in turns)' <<< '{}'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
