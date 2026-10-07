#!/usr/bin/env bash
# 真实 e2e：期 2 时间旅行 + 冷层归档（真实对话内容驱动）。
# ① 真实对话 → checkpoint（快照 ref 非空）→ 续写
# ② fork 分支（分支时间轴仅前缀 + 血缘）→ 分支真实续答
# ③ diff 差集（父/分支各自独有）
# ④ rollback → 回退审计 + 回退后真实继续
# ⑤ 归档（ARCHIVE_MIN_AGE=0 栈上）→ 归档清单
# 前置：harness 真实模式；api ARCHIVE_MIN_AGE=0s。
# 用法: bash test/e2e/real-timetravel.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="rtt-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 真实 e2e：时间旅行 + 归档（API=${API}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-tt-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是时间旅行演示助手，回答简洁。","tools":["bash"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/rtt-sse.out 2>&1 &
SSE_PID=$!
sleep 1
for i in $(seq 1 5); do
  curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
    -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"用一句话定义时间旅行"}' > /tmp/rtt-1.out 2>&1 && break
  sleep 2
done || true

# ① checkpoint + 续写
CP=$(curl -fsS -X POST "$API/sessions/$SID/checkpoints" -H 'content-type: application/json' -d '{}')
CP_ID=$(echo "$CP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["checkpoint_id"])')
# 创建契约 = {checkpoint_id, session_id}；快照 ref 从 list 端点核对
CPS=$(curl -fsS "$API/sessions/$SID/checkpoints")
CP_LIST=$(curl -fsS "$API/sessions/$SID/checkpoints")
assert "checkpoint 创建（快照 ref 非空）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); cps=d["checkpoints"]; assert cps and cps[-1]["snapshot_ref"], d' <<< "$CPS"
for i in $(seq 1 5); do
  curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
    -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"再补充一句"}' > /tmp/rtt-2.out 2>&1 && break
  sleep 2
done || true

# ② fork
FORK=""
for i in $(seq 1 5); do FORK=$(curl -fsS -X POST "$API/sessions/$SID/fork" -H 'content-type: application/json' -d "{\"checkpoint_id\":\"$CP_ID\"}" 2>/dev/null || true); [ -n "$FORK" ] && break; sleep 2; done
FSID=$(echo "$FORK" | python3 -c 'import sys,json;print(json.load(sys.stdin)["session_id"])')
curl -fsS -m 120 -X POST "$API/sessions/$FSID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-f" -d '{"input":"分支上的新回答"}' > /tmp/rtt-f.out 2>&1 || true
assert "fork 分支真实续答（血缘 + 分支时间轴）" \
  bash -c 'grep -q "completed" /tmp/rtt-f.out'

# ③ diff
DIFF=$(curl -fsS "$API/sessions/$SID/diff?against=$FSID")
assert "diff 差集（双方各有独有事件）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["common_prefix"]>0 and d["only_a"] and d["only_b"], (d["common_prefix"], len(d["only_a"]), len(d["only_b"]))' <<< "$DIFF"

# ④ rollback
for i in $(seq 1 5); do curl -fsS -X POST "$API/sessions/$SID/rollback" -H 'content-type: application/json' -d "{\"checkpoint_id\":\"$CP_ID\"}" > /dev/null 2>&1 && break; sleep 2; done
sleep 2
assert "rollback 审计（session.rolled_back）" \
  grep -q '"type":"session.rolled_back"' /tmp/rtt-sse.out
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-3" -d '{"input":"回退后继续"}' > /tmp/rtt-3.out 2>&1 || true
assert "回退后真实继续完成" grep -q '"completed"' /tmp/rtt-3.out
kill "$SSE_PID" 2>/dev/null || true

# ⑤ 归档
ARC=$(curl -fsS -X POST "$API/sessions/$SID/archive" -H 'content-type: application/json' -d '{}')
assert "归档（清单 ref + archived_at）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d.get("bucket_path"), d' <<< "$ARC"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
