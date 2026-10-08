#!/usr/bin/env bash
# 期 4 §A 规模验证：千万级事件生成 + 回放计时 + 分区裁剪实证。
#   ① 热表 events 批量 COPY 生成 N 万事件（默认 1000 万，SCALE_N 可调）
#   ② 回放演练：按水位全量扫（ListEventsAfterID 语义）计时断言 < 预算
#   ③ archive_events 按月分区插入 + EXPLAIN 分区裁剪断言（仅扫单分区）
# 前置：postgres 运行（compose 或本机）；STORE 测试库 URL 默认本地。
# 用法: SCALE_N=10000000 bash test/scale/gen-events.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="/opt/homebrew/bin:$PATH"
DB='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable'
N="${SCALE_N:-10000000}"
RUN_ID="scale-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() {
  local desc="$1"; shift
  if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

PSQL() { docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "$1"; }

echo "== 期 4 §A 规模验证（N=${N}，RUN_ID=${RUN_ID}）=="

# ① 批量生成（COPY 直插：单会话 1000 万事件——时间轴规模化上限压力）
SID="s_$RUN_ID"
# 外键地基：orgs/agents/sessions 行（COPY 前建——events_session_id_fkey 实证）
PSQL "INSERT INTO orgs (id, name) VALUES ('o_$RUN_ID', 'scale') ON CONFLICT DO NOTHING" > /dev/null
PSQL "INSERT INTO agents (id, org_id, name, config, version) VALUES ('a_$RUN_ID', 'o_$RUN_ID', 'scale', '{}'::jsonb, 1) ON CONFLICT DO NOTHING" > /dev/null
PSQL "INSERT INTO sessions (id, org_id, agent_id, status) VALUES ('$SID', 'o_$RUN_ID', 'a_$RUN_ID', 'ready') ON CONFLICT DO NOTHING" > /dev/null
python3 - "$SID" "$N" > /tmp/scale-events.csv <<'PYEOF'
import sys
sid, n = sys.argv[1], int(sys.argv[2])
out = sys.stdout.write
chunk = []
for i in range(n):
    chunk.append(f"{sid}\tr_{i%100}\t{i}\trun.completed\t{{\"v\":1}}\t2026-10-15T00:00:{i%60:02d}Z\n")
    if len(chunk) == 10000:
        out("".join(chunk)); chunk.clear()
if chunk:
    out("".join(chunk))
PYEOF
START=$(date +%s)
docker cp /tmp/scale-events.csv chronotope-postgres-1:/tmp/scale-events.csv > /dev/null 2>&1
docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -c \
  "COPY events (session_id, run_id, seq, type, payload, at) FROM '/tmp/scale-events.csv'" > /dev/null 2>&1
DUR=$(( $(date +%s) - START ))
echo "  生成 ${N} 事件耗时 ${DUR}s"
assert "千万级事件生成（COPY < 120s）" test "$DUR" -lt 120

# ② 回放演练：水位推进全量扫（ListEventsAfterID 语义）计时断言
START=$(date +%s)
COUNT=$(PSQL "SELECT count(*) FROM events WHERE id > 0")
DUR2=$(( $(date +%s) - START ))
echo "  全量回放扫描 ${COUNT} 行耗时 ${DUR2}s"
assert "回放扫描计数正确（= N + 历史）" bash -c '[ "$1" -ge '"$N"' ]' _ "$COUNT"
assert "回放扫描计时 < 60s（索引序扫）" test "$DUR2" -lt 60

# ③ archive_events 分区插入 + 裁剪实证（按月路由到单分区）
docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -c \
  "COPY archive_events (session_id, run_id, seq, type, payload, at) FROM '/tmp/scale-events.csv'" > /dev/null 2>&1
PLAN=$(PSQL "EXPLAIN (FORMAT JSON) SELECT count(*) FROM archive_events WHERE at >= '2026-10-01' AND at < '2026-11-01'")
assert "分区裁剪实证（EXPLAIN 仅扫 2026_10 分区）" \
  bash -c 'echo "$1" | grep -q "archive_events_2026_10" && ! echo "$1" | grep -q "archive_events_default"' _ "$PLAN"

# 清理演练数据（保测试库轻量）
PSQL "DELETE FROM events WHERE session_id='$SID'" > /dev/null
PSQL "DELETE FROM archive_events WHERE session_id='$SID'" > /dev/null
PSQL "DELETE FROM sessions WHERE id='$SID'" > /dev/null
PSQL "DELETE FROM agents WHERE id='a_$RUN_ID'" > /dev/null
PSQL "DELETE FROM orgs WHERE id='o_$RUN_ID'" > /dev/null
PSQL "DELETE FROM sessions WHERE id='$SID'" > /dev/null
PSQL "DELETE FROM agents WHERE id='a_$RUN_ID'" > /dev/null
PSQL "DELETE FROM orgs WHERE id='o_$RUN_ID'" > /dev/null
rm -f /tmp/scale-events.csv

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
