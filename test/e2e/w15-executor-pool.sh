#!/usr/bin/env bash
# 期 4 §B：多宿主 executor 池演练——双 executor 负载分布 + 归属路由 + 下线恢复。
#   ① 双 executor（不同端口/不同 ID）注册 → 心跳入表
#   ② 多会话 run → 沙箱分布到两 executor（executors 负载分布断言）
#   ③ kill 一个 executor → 新会话 run 仍完成（存活者承接——无卷会话恢复链）
# 前置：harness fake plain + api/worker 运行（compose 或本地）。
# 用法: bash test/e2e/w15-executor-pool.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="/opt/homebrew/bin:$PATH"
API="${1:-http://localhost:8080}"
DB='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'
RUN_ID="w15-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}
PSQL() { docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "$1"; }

echo "== 期 4 §B 多宿主 executor 池（API=${API}，RUN_ID=${RUN_ID}）=="

# harness fake 脚本模式（write_file 触发沙箱——plain 模式不调工具，0/0 实证）
pkill -f 'uvicorn app.main:app' 2>/dev/null || true
sleep 1
(cd harness && nohup env HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/pool.txt","content":"pool"}}},{"final":"写入完成。"}]' uv run uvicorn app.main:app --port 8000 > /tmp/w15-harness.log 2>&1 &)
sleep 4
curl -fsS localhost:8000/healthz > /dev/null && echo "harness 脚本模式就绪"

# ① 双 executor（本地二进制，不同端口/ID）——worker 经注册表路由
go build -o bin/chronotope-executor ./cmd/executor
pkill -f 'chronotope-executor' 2>/dev/null || true
sleep 1
nohup env DATABASE_URL="$DB" EXECUTOR_ID="exec-a" EXECUTOR_ADDR=":9082" ./bin/chronotope-executor -addr :9082 > /tmp/w15-exec-a.log 2>&1 &
nohup env DATABASE_URL="$DB" EXECUTOR_ID="exec-b" EXECUTOR_ADDR=":9083" ./bin/chronotope-executor -addr :9083 > /tmp/w15-exec-b.log 2>&1 &
sleep 3
curl -fsS localhost:9082/healthz >/dev/null && curl -fsS localhost:9083/healthz >/dev/null && echo "双 executor 就绪"
for i in $(seq 1 10); do
  N=$(PSQL "SELECT count(*) FROM executors WHERE status='up'" 2>/dev/null || echo 0)
  [ "$N" = "2" ] && break
  sleep 1
done
assert "双 executor 注册入表（心跳新鲜）" test "$(PSQL "SELECT count(*) FROM executors WHERE status='up'")" = "2"

# ② 负载分布：6 会话 run → 沙箱分属两 executor
AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"pool-agent","config":{"model":"claude-sonnet-4-6","instructions":"你必须先调用 write_file 工具（参数 path=\"/workspace/pool.txt\"，content=\"pool\"），然后一句话回答。","tools":["write_file"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
for i in 1 2 3 4 5 6; do
  S=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
  curl -fsS -m 120 -X POST "$API/sessions/$S/runs" -H 'content-type: application/json' \
    -H "Idempotency-Key: $RUN_ID-$i" -d '{"input":"写入文件"}' > /dev/null 2>&1 || true
done
sleep 3
A_COUNT=$(PSQL "SELECT count(*) FROM sandboxes WHERE executor_id='exec-a'")
B_COUNT=$(PSQL "SELECT count(*) FROM sandboxes WHERE executor_id='exec-b'")
echo "  沙箱归属分布: exec-a=$A_COUNT exec-b=$B_COUNT"
assert "负载分布到两 executor" bash -c '[ "$1" -gt 0 ] && [ "$2" -gt 0 ]' _ "$A_COUNT" "$B_COUNT"

# ③ 下线恢复：kill exec-a → 新会话 run 仍完成（存活者承接）
pkill -f 'chronotope-executor -addr :9082' 2>/dev/null || true
sleep 2
S2=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
OUT=$(curl -fsS -m 120 -X POST "$API/sessions/$S2/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-post" -d '{"input":"再写一次"}' 2>/dev/null || true)
assert "executor 下线后新会话 run 完成（存活者承接）" \
  bash -c 'echo "$1" | grep -q completed' _ "$OUT"

pkill -f 'chronotope-executor' 2>/dev/null || true
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
