#!/usr/bin/env bash
# 评审 #7 e2e：快照含卷恢复（Tier2——「一周前会话今天还能继续」的机制）。
# 前置：harness 脚本模式①（write_file 写 /workspace/a.txt）；api/worker/executor
#       运行中。harness 的脚本切换由调用方（demo 阶段）负责。
# 用法: bash test/e2e/w2-snapshot.sh [API_URL]
set -euo pipefail

API="${1:-http://localhost:8080}"
RUN_ID="snap-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

echo "== 快照含卷恢复 e2e（API=${API}，RUN_ID=${RUN_ID}）=="

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"snap","config":{"model":"claude-sonnet-4-6","instructions":"i","tools":["write_file","read_file","bash"],"environment":{"sandbox":{"image":"python:3.11-slim","limits":{"cpu":"1","mem":"256m"}}},"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ① run1：写文件（harness 脚本①）
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"写入快照文件"}' > /dev/null
curl -fsS -N -m 3 "$API/sessions/$SID/events?after=0" > /tmp/snap-sse1.out 2>&1 || true
SB_ID=$(grep -a -o '"sandbox_id":"sb_[a-f0-9]*"' /tmp/snap-sse1.out | head -1 | cut -d'"' -f4)
assert "run1 完成且沙箱已创建" test -n "$SB_ID"

# ② 快照（executor 端点）→ ref 必须编码卷 tar（评审 #7：named volume 不进镜像）
REF=$(curl -fsS -X POST "localhost:9082/sandboxes/$SB_ID/snapshot" | python3 -c 'import sys,json;print(json.load(sys.stdin)["snapshot_ref"])')
assert "快照 ref 编码镜像|卷tar" bash -c 'echo "$1" | grep -q "|"' _ "$REF"

# ③ 销毁沙箱（模拟「一周后回收」）
curl -fsS -X DELETE "localhost:9082/sandboxes/$SB_ID" > /dev/null

# ④ run2：read_file（harness 脚本②已由调用方切换）→ ensureSandbox 从快照重建
# 后台提交 run2（ingress 阻塞至 run 终态；断言走事件流——重放交错下更稳）
curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"读取快照文件"}' > /tmp/snap-run2.out 2>&1 &
RUN_PID=$!
for i in $(seq 1 40); do
  curl -fsS -N -m 2 "$API/sessions/$SID/events?after=0" > /tmp/snap-sse2.out 2>&1 || true
  grep -q '快照前内容' /tmp/snap-sse2.out && break
  sleep 3
done
kill "$RUN_PID" 2>/dev/null || true
assert "run2 触发（run.started 事件）" \
  grep -q '"type":"run.started"' /tmp/snap-sse2.out
assert "恢复后 read_file 读到快照内容（沙箱重建闭环）" \
  grep -q '快照前内容' /tmp/snap-sse2.out

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
