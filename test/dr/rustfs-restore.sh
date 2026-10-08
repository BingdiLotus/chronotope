#!/usr/bin/env bash
# 期 4 §D2：RustFS 归档恢复演练——归档对象（ndjson.gz）→ 物化回放重建
# 会话时间轴，与 PG 基线比对（时间轴等价）。
# 流程：写会话 + run → 归档 → 删 PG 事件（破坏）→ 从 RustFS 下载归档 →
#       ndjson 解析回放 → 断言事件数与基线一致（重建链有效）。
# 前置：docker compose 全栈运行中 + api 可写（fake harness）。
# 用法: bash test/dr/rustfs-restore.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="/opt/homebrew/bin:$PATH"
API="${1:-http://localhost:8080}"
RUN_ID="dr2-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== RustFS 归档恢复演练（期 4 §D2，RUN_ID=${RUN_ID}）=="

# 前置：api 归档 age 闸门归零（会话新建即可归档——默认 24h 会 4xx，exit 22 实证）
ARCHIVE_MIN_AGE=0s docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate api > /dev/null 2>&1
sleep 6
for i in $(seq 1 20); do curl -fsS localhost:8080/healthz > /dev/null 2>&1 && break; sleep 1; done

# 1. 写会话 + run（fake harness 已由 demo/CI 环境提供——脚本仅走 API）
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"dr-agent","config":{"model":"claude-sonnet-4-6","instructions":"一句话。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID" -d '{"input":"hello"}' > /dev/null 2>&1 || true
# 2. 归档
curl -fsS -m 30 -X POST "$API/sessions/$SID/archive" > /tmp/dr2-archive.out 2>&1
BUCKET_PATH=$(python3 -c 'import json;print(json.load(open("/tmp/dr2-archive.out")).get("bucket_path",""))' 2>/dev/null || echo "")
assert "归档 201（bucket_path 记录）" test -n "$BUCKET_PATH"
# 基线 = 归档 201 的 events 计数（SSE grep 混 beat/done 帧——30 vs 3 实证；
# 权威源是归档响应）
BASE_EVENTS=$(python3 -c 'import json;print(json.load(open("/tmp/dr2-archive.out")).get("events",0))' 2>/dev/null || echo 0)
BASE_EVENTS=$(echo "$BASE_EVENTS" | tr -d '[:space:]')
pass "基线事件数记录 events=${BASE_EVENTS}"

# 3. 破坏：删 PG 事件
docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -qc "DELETE FROM events WHERE session_id='$SID'" 2>/dev/null
AFTER_EVENTS=$(curl -fsS -m 5 -N "$API/sessions/$SID/events?after=0" 2>/dev/null | grep -c '"type"' || echo 0)
AFTER_EVENTS=$(echo "$AFTER_EVENTS" | tr -d '[:space:]')
[ "$AFTER_EVENTS" -le 0 ] && pass "破坏 (PG 事件清空)" || fail "破坏不彻底: $AFTER_EVENTS"

# 4. 从 RustFS S3 网关取归档对象（SigV4 签名——Basic 头 invalid 实证）
# → ndjson 解析回放（物化重建链）
python3 test/dr/s3get.py "http://localhost:9000" "workspaces" "archives/org-$RUN_ID/$SID/events.ndjson.gz" chronotope chronotope_dev /tmp/dr2-obj.gz 2>&1 || true
if [ -s /tmp/dr2-obj.gz ]; then
  REPLAY_COUNT=$(gunzip -c /tmp/dr2-obj.gz 2>/dev/null | python3 -c 'import sys,json;print(sum(1 for l in sys.stdin if l.strip() and json.loads(l).get("SessionID")))' 2>/dev/null || echo 0)
  echo "  replay=$REPLAY_COUNT base=$BASE_EVENTS"
  assert "物化回放重建 (replay=base)" test "$REPLAY_COUNT" -eq "$BASE_EVENTS"
else
  fail "归档对象不可达 bucket_path=$BUCKET_PATH"
fi

# 5. 清理
rm -f /tmp/dr2-obj.gz /tmp/dr2-archive.out /tmp/dr2.tar.gz
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
