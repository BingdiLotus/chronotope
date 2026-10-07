#!/usr/bin/env bash
# 时间旅行 e2e（正式版架构 期 2）：checkpoint → 续写 → fork 分支时间轴 →
# diff 差集 → rollback 投影截断（事件轴真相不可变 + rollback 审计事件）。
# 前置：harness fake（主脚本 write_file / ALT 脚本 read_file）。
# 用法: bash test/e2e/w10-timetravel.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="tt-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 时间旅行 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

ORG="org-$RUN_ID"
AGENT=$(curl -fsS -X POST "$API/orgs/$ORG/agents" -H 'content-type: application/json' \
  -d '{"name":"tt-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是时间旅行助手。","tools":["write_file","read_file"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ① 首轮（主脚本：write_file）→ 完成
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"写文件"}' > /tmp/tt-1.out 2>&1
assert "首轮完成（run.completed）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/tt-1.out

# ② checkpoint（时间坐标：seq 水位 + 沙箱快照引用）
CP=$(curl -fsS -X POST "$API/sessions/$SID/checkpoints" -H 'content-type: application/json' -d '{}')
CPID=$(echo "$CP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["checkpoint_id"])')
CPS=$(curl -fsS "$API/sessions/$SID/checkpoints")
assert "checkpoint 落库（时间坐标 + 沙箱快照引用非空）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); cps=d["checkpoints"]; assert len(cps)==1 and cps[0]["seq"]>0 and cps[0]["snapshot_ref"]!="", cps' <<< "$CPS"

# ③ 次轮（ALT 脚本：read_file）→ 完成（checkpoint 之后的新事实）
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"读文件"}' > /tmp/tt-2.out 2>&1
assert "次轮完成（checkpoint 后新事实）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/tt-2.out

# ④ fork：分支时间轴 = 前缀 + session.forked，无次轮事件
FORK=$(curl -fsS -X POST "$API/sessions/$SID/fork" -H 'content-type: application/json' \
  -d "{\"checkpoint_id\":\"$CPID\"}")
FSID=$(echo "$FORK" | python3 -c 'import sys,json;print(json.load(sys.stdin)["session_id"])')
sleep 2
FORK_EVENTS=$(curl -sS -N "$API/sessions/$FSID/events?after=0" --max-time 3 2>/dev/null | grep '^data: ' | head -20 || true)
# fork 空间面（期 2 遗留 #1）：工作区索引复制——分支沙箱可从内容寻址恢复
assert "fork 空间面复制（workspace_files 索引随分支）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM workspace_files WHERE session_id='$FSID'\" | grep -qE '^[1-9]'"
assert "fork 血缘（forked_from + at_seq）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["forked_from"]=="'"$SID"'" and d["at_seq"]>0, d' <<< "$FORK"
assert "fork 时间轴 = 前缀 + session.forked（仅首轮 completed，无次轮）" \
  bash -c 'echo "$1" | grep -q "session.forked" && [ "$(echo "$1" | grep -c "run.completed")" -eq 1 ]' _ "$FORK_EVENTS"

# ⑤ diff：父 vs fork → 公共前缀 + 各自后缀
DIFF=$(curl -fsS "$API/sessions/$SID/diff?against=$FSID")
assert "diff 差集（公共前缀 + 双方独有）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["common_prefix"]>=1 and len(d["only_a"])>=1 and len(d["only_b"])>=1, d' <<< "$DIFF"

# ⑥ rollback：投影截断 + rollback 事件（真相不可变审计）
curl -fsS -X POST "$API/sessions/$SID/rollback" -H 'content-type: application/json' \
  -d "{\"checkpoint_id\":\"$CPID\"}" > /dev/null
sleep 1
ROLL_EVENTS=$(curl -sS -N "$API/sessions/$SID/events?after=0" --max-time 3 2>/dev/null | grep '^data: ' | head -40 || true)
assert "rollback 事件追加（真相不可变审计：事件轴含 session.rolled_back）" \
  bash -c 'echo "$1" | grep -q "session.rolled_back"' _ "$ROLL_EVENTS"
assert "rollback 后会话可继续（新 run 正常完成）" \
  bash -c 'curl -fsS -m 60 -X POST '"$API"'/sessions/'"$SID"'/runs -H "content-type: application/json" -H "Idempotency-Key: '"$RUN_ID"'-3" -d "{\"input\":\"回退后继续\"}" | python3 -c "import sys,json; assert json.load(sys.stdin)[\"status\"]==\"completed\""'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
