#!/usr/bin/env bash
# 真实 e2e：W2 沙箱工具 + W3 定时唤醒 + W8 交付（全部真实模型驱动）。
# ① 真实模型依次 write_file → bash cat → read_file（沙箱真实执行）
# ② checkpoint 快照 → 销毁沙箱 → 新 run 从快照恢复继续（真实续答）
# ③ schedule 定时唤醒（durable timer 到点 → 真实模型应答）
# ④ outbox 交付（run 完成 → 交付行 + 投递回执）
# 前置：harness 真实模式（.env 密钥 + litellm）。
# 用法: bash test/e2e/real-tools.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="rtool-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 真实 e2e：沙箱工具/快照/定时/交付（API=${API}，RUN_ID=${RUN_ID}）=="

# --- ① 真实模型驱动沙箱工具 ---
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"real-tool-agent","config":{"model":"claude-sonnet-4-6","instructions":"你必须依次调用工具（每次调用都要完整填写参数）：1) write_file，参数 path=\"/workspace/real.txt\"，content=\"真实沙箱内容\"；2) bash，参数 command=\"cat /workspace/real.txt\"；3) read_file，参数 path=\"/workspace/real.txt\"。全部完成后用一句话复述文件内容。","tools":["write_file","bash","read_file"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID/events?after=0" > /tmp/rtool-sse.out 2>&1 &
SSE_PID=$!
sleep 1
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"开始执行"}' > /tmp/rtool-run.out 2>&1 &
RUN_PID=$!
wait "$RUN_PID" || true
sleep 2
kill "$SSE_PID" 2>/dev/null || true

assert "真实 run 完成（工具链全绿）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/rtool-run.out
assert "sandbox.exec 真实执行（write/bash/read 均落库）" \
  python3 -c 'import sys,json; data=[json.loads(l[6:]) for l in open("/tmp/rtool-sse.out") if l.startswith("data: ")]; ex=[e for e in data if e["type"]=="sandbox.exec"]; assert len(ex)>=3, len(ex)' <<< '{}'
assert "真实模型终答引用文件内容" \
  bash -c 'grep -q "真实沙箱内容" /tmp/rtool-run.out'

# --- ② 快照恢复（真实续答）---
CP=""
for i in $(seq 1 5); do CP=$(curl -fsS -X POST "$API/sessions/$SID/checkpoints" -H 'content-type: application/json' -d '{}' 2>/dev/null || true); [ -n "$CP" ] && break; sleep 2; done
SB=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1")
curl -fsS -X DELETE "localhost:9082/sandboxes/$SB" > /dev/null 2>&1 || true
sleep 2
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"再读一次 /workspace/real.txt 并复述内容"}' > /tmp/rtool-run2.out 2>&1 || true
assert "快照恢复后真实续答（复述原内容）" \
  bash -c 'grep -q "真实沙箱内容" /tmp/rtool-run2.out'

# --- ③ 定时唤醒 ---
SID2=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -N "$API/sessions/$SID2/events?after=0" > /tmp/rtool-sch.out 2>&1 &
SCH_SSE=$!
sleep 1
curl -fsS -X POST "$API/sessions/$SID2/schedules" -H 'content-type: application/json' \
  -d '{"delay_ms":5000,"payload":{"input":"定时任务：回复定时唤醒成功"}}' > /tmp/rtool-sch-create.out
for i in $(seq 1 60); do grep -q '"type":"session.woken"' /tmp/rtool-sch.out 2>/dev/null && break; sleep 2; done
kill "$SCH_SSE" 2>/dev/null || true
assert "定时唤醒：session.woken + 真实应答" \
  bash -c 'grep -q "session.woken" /tmp/rtool-sch.out && grep -q "定时唤醒成功" /tmp/rtool-sch.out'

# --- ④ outbox 交付 ---
SID3=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 120 -X POST "$API/sessions/$SID3/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-4" -d '{"input":"回复：交付测试完成"}' > /tmp/rtool-run4.out 2>&1 || true
DLV=$(curl -fsS "$API/sessions/$SID3/deliveries")
assert "交付行生成（run_completed + 未投递）" \
  bash -c 'echo "$1" | grep -q "run_completed" && ! echo "$1" | grep -q "delivered_at"' _ "$DLV"
DID=$(echo "$DLV" | python3 -c 'import sys,json;print(json.load(sys.stdin)["deliveries"][0]["id"])')
assert "投递回执 204" \
  bash -c 'curl -sS -o /dev/null -w "%{http_code}" -X POST '"$API"'/sessions/'"$SID3"'/deliveries/'"$DID"'/ack | grep -q 204'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
