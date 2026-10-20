#!/usr/bin/env bash
# W1 验收 e2e：建 agent → 建 session → 提交任务 → 对话完成 → SSE 实时收事件。
# 前置：postgres/restate 已起（make dev-up）、worker 已注册（make register-worker）、
#       api/worker/harness 三个服务运行中（本地：见 README 快速开始）。
# 用法: bash test/e2e/w1-loop.sh [API_URL]
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/http.sh"

API="${1:-http://localhost:8080}"
RUN_ID="e2e-$(date +%s)-$$"
AUTH_HEADERS=(-H 'Accept: */*')
if [ -n "${E2E_API_TOKEN:-}" ]; then AUTH_HEADERS+=(-H "Authorization: Bearer $E2E_API_TOKEN"); fi
SSE_FILE=$(mktemp)
RESUME_FILE=$(mktemp)
SSE_PID=""
cleanup() {
  if [ -n "$SSE_PID" ]; then kill "$SSE_PID" 2>/dev/null || true; fi
  rm -f "$SSE_FILE" "$RESUME_FILE"
}
trap cleanup EXIT
PASS=0; FAIL=0

pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
# assert <desc> <cmd...>：cmd 从 stdin 读取待断言 JSON（heredoc 注入），退出码 0 即通过
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== W1 闭环 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

# 1. 创建 agent（config 版本化）
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-agent" "${AUTH_HEADERS[@]}" \
  -d '{"name":"e2e-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是演示助手。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
assert "创建 agent（返回 id 与 config version=1）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["id"] and d["config"]["version"]==1' <<< "$AGENT"

# 2. 创建 session → ready
SESS=$(curl -fsS -X POST "$API/agents/$AID/sessions" -H "Idempotency-Key: $RUN_ID-session" "${AUTH_HEADERS[@]}")
SID=$(echo "$SESS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
assert "创建 session（status=ready）" \
  python3 -c 'import sys,json; assert json.load(sys.stdin)["status"]=="ready"' <<< "$SESS"

# 3. SSE 订阅（后台）——从 after=0 开始
curl -fsS -N "$API/sessions/$SID/events?after=0" "${AUTH_HEADERS[@]}" > "$SSE_FILE" 2>&1 &
SSE_PID=$!
sleep 1

# 4. 提交任务（幂等键）
ADMISSION=$(submit_run "$API" "$SID" "$RUN_ID" '{"input":"你好，介绍一下你自己"}')
assert "HTTP 202 耐久接纳（稳定 run_id，queued，不伪造 final）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["run_id"] and d["status"]=="queued" and "final" not in d' <<< "$ADMISSION"
RID1=$(python3 -c 'import sys,json;print(json.load(sys.stdin)["run_id"])' <<< "$ADMISSION")
RUN=$(wait_run "$API" "$RID1" 180)
assert "GET run 查询终态（status=completed + final 非空）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed" and d["final"] and d["usage"] is not None' <<< "$RUN"

# 5. 幂等重放：同键 → 同 run_id + idempotent 标记
RUN2=$(curl -fsS -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" "${AUTH_HEADERS[@]}" -d '{"input":"你好，介绍一下你自己"}')
assert "幂等重放返回同一 run_id 且标记 idempotent" \
  python3 -c "import sys,json; d=json.load(sys.stdin); assert d['run_id']=='$RID1' and d['idempotent'] is True" <<< "$RUN2"

# 6. SSE 事件：run.started → llm.call → run.completed 顺序到达
sleep 2
kill "$SSE_PID" 2>/dev/null || true
EVENTS=$(grep -c '^data: ' "$SSE_FILE" || true)
assert "SSE 收到事件流（≥3 条）" test "$EVENTS" -ge 3
assert "事件序列含 run.started" grep -q '"type":"run.started"' "$SSE_FILE"
assert "事件序列含 llm.call" grep -q '"type":"llm.call"' "$SSE_FILE"
assert "事件序列含 run.completed" grep -q '"type":"run.completed"' "$SSE_FILE"
ORDER=$(grep -a -o '"type":"run\.[a-z]*"\|"type":"llm.call"' "$SSE_FILE" | paste -sd, -)
assert "事件顺序 run.started→llm.call→run.completed" \
  python3 -c "import sys; s=sys.stdin.read(); assert 'run.started' in s and 'llm.call' in s and 'run.completed' in s and s.index('run.started') < s.index('llm.call') < s.index('run.completed')" <<< "$ORDER"
echo "  事件序列: $ORDER"

# 7. 时间轴回看：GET session 返回最近事件
GETS=$(curl -fsS "$API/sessions/$SID" "${AUTH_HEADERS[@]}")
assert "会话时间轴可回看（recent_events ≥ 3）" \
  python3 -c 'import sys,json; assert len(json.load(sys.stdin)["recent_events"]) >= 3' <<< "$GETS"

# 8. 断线续读：after=第二条事件的 seq 重连，只收后续事件
AFTER=$(echo "$GETS" | python3 -c 'import sys,json; print(json.load(sys.stdin)["recent_events"][1]["seq"])')
curl -fsS -N -m 3 "$API/sessions/$SID/events?after=$AFTER" "${AUTH_HEADERS[@]}" > "$RESUME_FILE" 2>&1 || true
assert "断线续读（after=seq 收到后续事件且不含已读事件）" \
  python3 -c "import sys,json; rows=[json.loads(l[6:]) for l in sys.stdin if l.startswith('data: ')]; assert rows and all(r['seq'] > $AFTER for r in rows)" < "$RESUME_FILE"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
