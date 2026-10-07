#!/usr/bin/env bash
# 真实 e2e 缺陷回归（w14）：8 项 fake 绿真实红缺口的固化断言——
#   ① 快照产物独立存留（checkpoint → destroy 后 SnapshotRoot tar 仍在 + 恢复链成功）
#   ② fork 落库即初始化（fork → 立即 run 完成——真实 e2e 502 回归）
#   ③ knowledge content-only 写入（Embedder 接线——422 回归）+ fake 挂载注入
#   ④ 跨 run 消息 tool_call_id 配对（messages 表 tool 消息结构——Anthropic 400 回归）
#   ⑤ write_file 恢复链（销毁 → run2 write_file 重建成功——包装丢失回归）
# 前置：harness fake 模式（各段脚本切换）；compose executor 已挂宿主目录。
# 用法: bash test/e2e/w14-regression.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/../.."

API="${1:-http://localhost:8080}"
EXEC="${EXECUTOR_URL:-http://localhost:9082}"
RUN_ID="w14-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}
harness_script() { # $1=HARNESS_FAKE_SCRIPT 值
  HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT="$1" \
    docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness > /dev/null 2>&1
  for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
}

echo "== 真实 e2e 缺陷回归（API=${API}，RUN_ID=${RUN_ID}）=="

# ─── ① 快照产物独立存留 + 恢复链 ───
harness_script '[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/w14.txt","content":"回归内容"}}},{"tool_call":{"name":"bash","arguments":{"command":"cat /workspace/w14.txt"}}},{"final":"写入完成。"}]'
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"w14-agent","config":{"model":"claude-sonnet-4-6","instructions":"回归助手。","tools":["write_file","bash","read_file"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"写入文件"}' > /tmp/w14-1.out 2>&1 || true
CP=$(curl -fsS -X POST "$API/sessions/$SID/checkpoints" -H 'content-type: application/json' -d '{}')
SB=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID' AND status='ready' ORDER BY created_at DESC LIMIT 1")
curl -fsS -X DELETE "$EXEC/sandboxes/$SB" > /dev/null 2>&1 || true
sleep 2
assert "快照产物独立存留（destroy 后 SnapshotRoot tar 仍在）" \
  bash -c 'find /tmp/chronotope-snapshots -name volume.tar | grep -q volume.tar'
harness_script '[{"tool_call":{"name":"bash","arguments":{"command":"cat /workspace/w14.txt"}}},{"final":"读取完成。"}]'
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"再读文件"}' > /tmp/w14-2.out 2>&1 || true
assert "快照恢复链成功（续 run 完成）" grep -q '"status":"completed"' /tmp/w14-2.out

# ─── ② fork 落库即初始化 ───
CP_ID=$(echo "$CP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["checkpoint_id"])')
FORK=$(curl -fsS -X POST "$API/sessions/$SID/fork" -H 'content-type: application/json' \
  -d "{\"checkpoint_id\":\"$CP_ID\"}")
FSID=$(echo "$FORK" | python3 -c 'import sys,json;print(json.load(sys.stdin)["session_id"])')
curl -fsS -m 120 -X POST "$API/sessions/$FSID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-f" -d '{"input":"分支首跑"}' > /tmp/w14-f.out 2>&1 || true
assert "fork 后立即 run 完成（SessionState 已初始化——502 回归）" \
  grep -q '"status":"completed"' /tmp/w14-f.out

# ─── ③ knowledge content-only 写入 + 挂载注入 ───
harness_script ""
KN=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/knowledge" -H 'content-type: application/json' \
  -d '{"content":"回归知识：w14 挂载注入验证。"}')
assert "knowledge content-only 写入成功（Embedder 接线——422 回归）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["id"], d' <<< "$KN"
SID2=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 120 -X POST "$API/sessions/$SID2/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-k" -d '{"input":"用共享知识回答"}' > /tmp/w14-k.out 2>&1 || true
assert "fake 挂载注入（harness 载荷含知识内容）" \
  bash -c 'docker compose --env-file .env -f deploy/docker-compose.yml logs harness 2>/dev/null | grep -q "w14 挂载注入验证"'

# ─── ④ tool_call_id 配对（messages 表结构）───
docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \
  "SELECT count(*) FROM messages WHERE role='tool' AND content::text LIKE '%tool_call_id%'" > /tmp/w14-toolcall.out
assert "tool 消息带 tool_call_id（Anthropic 400 回归）" \
  grep -qE "^[1-9]" /tmp/w14-toolcall.out
# ─── ⑤ write_file 恢复链（销毁 → 重建）───
harness_script '[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/w14b.txt","content":"blob回归"}}},{"final":"写入完成。"}]'
SID3=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 120 -X POST "$API/sessions/$SID3/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-3" -d '{"input":"写文件"}' > /tmp/w14-3.out 2>&1 || true
SB3=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID3' AND status='ready' ORDER BY created_at DESC LIMIT 1")
curl -fsS -X DELETE "$EXEC/sandboxes/$SB3" > /dev/null 2>&1 || true
sleep 2
harness_script '[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/w14c.txt","content":"恢复后写入"}}},{"final":"恢复写入完成。"}]'
curl -fsS -m 120 -X POST "$API/sessions/$SID3/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-4" -d '{"input":"恢复后写文件"}' > /tmp/w14-4.out 2>&1 || true
assert "write_file 恢复链（销毁后重建成功——包装丢失回归）" \
  grep -q '"status":"completed"' /tmp/w14-4.out

harness_script ""
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
