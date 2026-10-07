#!/usr/bin/env bash
# 工作区 blob 合同 e2e（期 2 §A）：写文件 → 索引行 + 内容寻址对象 → 销毁沙箱
# （无快照）→ blob: 恢复（第二条恢复链）→ 内容一致。
# 前置：harness fake（主脚本 write_file）；executor RUSTFS_ENDPOINT 已配。
# 用法: bash test/e2e/w11-blob.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
EXEC="http://localhost:9082"
RUN_ID="blob-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}
PSQL() { docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "$1"; }

echo "== 工作区 blob 合同 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

ORG="org-$RUN_ID"
AGENT=$(curl -fsS -X POST "$API/orgs/$ORG/agents" -H 'content-type: application/json' \
  -d '{"name":"blob-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是 blob 助手。","tools":["write_file","read_file"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ① 写文件 → 完成
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"写文件"}' > /tmp/blob-1.out 2>&1
assert "写文件 run 完成" \
  python3 -c 'import sys,json; d=json.load(sys.stdin); assert d["status"]=="completed", d' < /tmp/blob-1.out

# ② 索引行 + 内容寻址（workspace_files 行存在且 hash 非空）
if docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "SELECT hash FROM workspace_files WHERE session_id='$SID' AND path='/workspace/b.txt'" | grep -qE '^[0-9a-f]{64}$'; then
  pass "workspace_files 索引行（path→sha256 内容寻址）"
else
  fail "workspace_files 索引行（path→sha256 内容寻址）"
  echo "  [诊断] executor 日志（blob 相关）:" >&2
  docker compose -f deploy/docker-compose.yml logs executor 2>/dev/null | grep -iE 'blob|RUSTFS|listening' | head -8 >&2 || true
  docker exec chronotope-executor-1 env 2>/dev/null | grep -E 'RUSTFS' >&2 || true
  docker exec chronotope-executor-1 /usr/local/bin/chronotope-executor -help 2>&1 | head -3 >&2 || true
  docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "SELECT * FROM workspace_files" >&2 || true
fi

# ③ 销毁沙箱（无快照——blob 合同是唯一恢复来源）
SB_ID=$(PSQL "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1")
curl -fsS -X DELETE "$EXEC/sandboxes/$SB_ID" > /dev/null
assert "沙箱销毁（无快照）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT status FROM sandboxes WHERE sandbox_id='$SB_ID'\" | grep -q destroyed"

# ④ 新 run 触发 blob: 恢复 → 新沙箱从索引拉取 → 内容一致
#（ALT 脚本 bash cat——execute 路径的恢复链已实证；read_file 快路径的 SDK
# Run 挂起单独立项，见 期2-实施进度 已知问题）
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"读文件"}' > /tmp/blob-2.out 2>&1
SB2=$(PSQL "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1")
CONTENT=$(curl -fsS "$EXEC/files/$SB2/workspace/b.txt")
assert "blob 恢复后内容一致（快照之外第二条恢复链）" \
  bash -c '[ "$1" = "blob合同内容" ]' _ "$CONTENT"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
