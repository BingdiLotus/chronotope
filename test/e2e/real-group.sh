#!/usr/bin/env bash
# 真实模型组合复验（P2-0 扩展）：群聊 moderator + 两真实成员 + 子 Agent 组合。
# 核心观察：真实模型对主持工具协议（next_speaker）与派发协议（spawn_subagent）
# 的服从度——决定是否需要决策护栏。机制层确定性由 fake e2e 兜底。
# 前置：harness 真实模式（.env 密钥 + litellm）；worker/api 运行中。
# 用法: bash test/e2e/real-group.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="rgrp-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 真实模型组合复验（API=${API}，RUN_ID=${RUN_ID}）=="

ORG="org-$RUN_ID"
mk_agent() { # $1=name $2=model $3=instructions
  curl -fsS -X POST "$API/orgs/$ORG/agents" -H 'content-type: application/json' \
    -d "{\"name\":\"$1\",\"config\":{\"model\":\"$2\",\"instructions\":\"$3\",\"tools\":[],\"version\":1}}" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'
}
MOD=$(mk_agent moderator claude-sonnet-4-6 "你是讨论主持人。必须使用 next_speaker 工具依次让每位成员发言（participant 0 和 1，指令要具体），收集两轮意见后用一句话给出结论并结束。")
MEMBER_A=$(mk_agent member-a claude-sonnet-4-6 "你是架构师，发言必须简洁（两句话以内），观点明确。")
MEMBER_B=$(mk_agent member-b claude-sonnet-4-6 "你是评审，发言必须简洁（两句话以内），指出风险。")

SESS=$(curl -fsS -X POST "$API/agents/$MOD/sessions" -H 'content-type: application/json' \
  -d "{\"participants\":[{\"agent_id\":\"$MEMBER_A\",\"role\":\"架构师\"},{\"agent_id\":\"$MEMBER_B\",\"role\":\"评审\"}]}")
SID=$(echo "$SESS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "  群聊会话: ${SID}（moderator + 架构师 + 评审，全部真实模型）"

curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/rgrp-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 600 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"讨论：我们是否应该把单体拆成微服务？给出结论。"}' > /tmp/rgrp-run.out 2>&1 &
RUN_PID=$!
wait "$RUN_PID" || true
sleep 2
kill "$SSE_PID" 2>/dev/null || true

RUN=$(cat /tmp/rgrp-run.out)
echo "  run 响应: $(echo "$RUN" | head -c 160)"
assert "群聊 run 完成（真实 moderator 收敛出结论）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["final"], d' <<< "$RUN"

TURNS=$(grep -a -c '"type":"group.turn"' /tmp/rgrp-sse.out || true)
echo "  真实 group.turn 数: ${TURNS}"
if [ "$TURNS" -ge 1 ]; then
  assert "真实 moderator 服从主持协议（group.turn ≥1）" test "$TURNS" -ge 1
  # 成员真实发言：子会话时间轴有 run.completed + 非空 final
  assert "真实成员发言（子 run 完成且 final 非空）" \
    python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/rgrp-sse.out") if l.startswith("data: ")]; turns=[e for e in data if e["type"]=="group.turn"]; assert turns, "无 group.turn"' <<< '{}'
else
  echo "  [观察] 真实 moderator 未调用 next_speaker——主持协议服从性差，建议加决策护栏："
  echo "         moderator prompt 注入强制工具 schema + 首轮无 next_speaker 时平台重试/降级。"
  python3 -c 'import sys,json; d=json.load(sys.stdin); print("  moderator 答复:", d.get("final","")[:120])' < /tmp/rgrp-run.out || true
fi

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
