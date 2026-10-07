#!/usr/bin/env bash
# 冷层归档 e2e（期 2 §B）：老会话归档 → RustFS 对象 + 清单行 + archived_at。
# 前置：api 以 ARCHIVE_MIN_AGE=0 RUSTFS_ENDPOINT 运行（归档启用且年龄闸门放开）。
# 用法: bash test/e2e/w12-archive.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="arc-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}
PSQL() { docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "$1"; }

echo "== 冷层归档 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

ORG="org-$RUN_ID"
AGENT=$(curl -fsS -X POST "$API/orgs/$ORG/agents" -H 'content-type: application/json' \
  -d '{"name":"arc-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是归档助手。","tools":[],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ① 会话产生事实（run 完成）
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"归档前对话"}' > /tmp/arc-1.out 2>&1
assert "run 完成（归档前事实）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/arc-1.out

# ② 归档（ARCHIVE_MIN_AGE=0 的 api）→ 201 + 清单
ARC=$(curl -fsS -X POST "$API/sessions/$SID/archive" -H 'content-type: application/json' -d '{}')
assert "归档 201（bucket_path + 计数）" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["bucket_path"] and d["events"]>0 and d["messages"]>0, d' <<< "$ARC"

# ③ 清单可见 + archived_at 标记
assert "归档清单可见（GET /archive）" \
  bash -c 'curl -fsS '"$API"'/sessions/'"$SID"'/archive | python3 -c "import sys,json; d=json.load(sys.stdin); assert d[\"bucket_path\"] and d[\"events_count\"]>0, d"'
assert "会话 archived_at 标记" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT archived_at IS NOT NULL FROM sessions WHERE id='$SID'\" | grep -q t"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
