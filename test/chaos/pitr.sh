#!/usr/bin/env bash
# W8 后置 chaos：PITR 恢复演练（备份 → 破坏 → 恢复 → 校验）。
# 前置：docker compose 全栈运行中（postgres 容器 chronotope-postgres-1）。
# 用法: bash test/chaos/pitr.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
PG="docker exec chronotope-postgres-1"
PSQL="$PG psql -U chronotope -d chronotope"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }

echo "== PITR 恢复演练（备份 → 破坏 → 恢复 → 校验）=="

# 1. 制造事实：先写哨兵 + 记录事件基线（备份必须**包含**哨兵——
# 恢复校验「哨兵回」才有语义；曾倒序致校验永远失败，CI 实证）
$PSQL -qc "INSERT INTO orgs (id, name) VALUES ('org_pitr_probe', 'probe') ON CONFLICT (id) DO UPDATE SET name='probe'"
EVENTS_BEFORE=$($PSQL -tAc "SELECT count(*) FROM events")
pass "哨兵写入 + 基线记录（events=${EVENTS_BEFORE}）"

# 2. 备份（pg_dump 全量，含哨兵；生产用 WAL 归档 PITR——演练目标是恢复流程本身）
docker exec chronotope-postgres-1 pg_dump -U chronotope -d chronotope -Fc -f /tmp/chronotope.dump
pass "pg_dump 全量备份（/tmp/chronotope.dump）"

# 3. 破坏：删哨兵 + 删全部事件（模拟事故）
$PSQL -qc "DELETE FROM orgs WHERE id='org_pitr_probe'; DELETE FROM events" 2>/dev/null || $PSQL -qc "DELETE FROM orgs WHERE id='org_pitr_probe'"
EVENTS_DAMAGED=$($PSQL -tAc "SELECT count(*) FROM events")
[ "$EVENTS_DAMAGED" -eq 0 ] && pass "事故模拟（events 清空）" || fail "事故模拟（events 未清空: $EVENTS_DAMAGED）"

# 4. 恢复前显式预删分区表（--clean 对分区表的删除顺序不可靠——019 分区
# 表引入后 pg_restore 的 ALTER ATTACH 冲突实证）；随后 --clean 恢复
$PSQL -qc "DROP TABLE IF EXISTS archive_events CASCADE" > /dev/null 2>&1 || true
# --clean 先删后建；pg_restore 到同一库——活跃连接可能干扰，
# 失败时日志留证并如实 fail，不再吞错假绿
if docker exec -i chronotope-postgres-1 pg_restore -U chronotope -d chronotope --clean --if-exists /tmp/chronotope.dump > /tmp/pitr-restore.log 2>&1; then
  pass "pg_restore 恢复（--clean --if-exists）"
else
  fail "pg_restore 恢复失败：$(tail -2 /tmp/pitr-restore.log | head -1)"
fi

# 5. 校验：哨兵回 + 事件数回基线
if $PSQL -tAc "SELECT count(*) FROM orgs WHERE id='org_pitr_probe'" | grep -q '^1$'; then
  pass "哨兵数据恢复"
else
  fail "哨兵数据未恢复"
fi
EVENTS_AFTER=$($PSQL -tAc "SELECT count(*) FROM events")
if [ "$EVENTS_AFTER" -ge "$EVENTS_BEFORE" ]; then
  pass "事件全量恢复（${EVENTS_AFTER} ≥ 基线 ${EVENTS_BEFORE}）"
else
  fail "事件未完全恢复（${EVENTS_AFTER} < ${EVENTS_BEFORE}）"
fi

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[[ "$FAIL" -eq 0 ]]
