#!/usr/bin/env bash
# 期 4 §D1：WAL 归档 PITR 演练——独立演练容器（不碰 compose 栈；
# 生产演练同理：恢复流程在隔离环境验证，不在业务库上演练）。
# 流程：演练 pg 起 → 开 WAL 归档 → basebackup → 哨兵写入（记目标时刻）→
#       目标后写入 → 破坏（删哨兵）→ 停库 → base 回填 + recovery 参数 →
#       回卷 → 断言「哨兵在、目标后不在」→ 清理演练容器与卷。
# 用法: bash test/dr/pitr.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="/opt/homebrew/bin:$PATH"
DRC="chronotope-dr-pg"   # 演练容器名
DRV="chronotope_dr_pgdata"
PG="docker exec $DRC"
PSQL="$PG psql -U postgres -d postgres"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }

echo "== WAL PITR 演练（期 4 §D1：隔离容器，点时间恢复）=="

cleanup() {
  docker rm -f "$DRC" > /dev/null 2>&1 || true
  docker volume rm -f "$DRV" > /dev/null 2>&1 || true
  rm -rf /tmp/dr-base /tmp/dr-pgwal
}
trap cleanup EXIT
cleanup

# 0. 起演练 pg（独立卷；WAL 归档开）
docker run -d --name "$DRC" -e POSTGRES_PASSWORD=dr -v "$DRV":/var/lib/postgresql/data postgres:16 > /dev/null
for i in $(seq 1 30); do $PSQL -tAc "SELECT 1" > /dev/null 2>&1 && break; sleep 1; done
$PSQL -qc "ALTER SYSTEM SET wal_level = replica" # ALTER SYSTEM 不能入事务块——分条执行
$PSQL -qc "ALTER SYSTEM SET archive_mode = on"
$PSQL -qc "ALTER SYSTEM SET archive_command = 'cp %p /var/lib/postgresql/data/pgwal/%f'"
$PG mkdir -p /var/lib/postgresql/data/pgwal && $PG chown postgres /var/lib/postgresql/data/pgwal
docker restart "$DRC" > /dev/null 2>&1
sleep 4
for i in $(seq 1 30); do $PSQL -tAc "SELECT 1" > /dev/null 2>&1 && break; sleep 1; done
ARCHIVE_ON=$($PSQL -tAc "SHOW archive_mode")
[ "$ARCHIVE_ON" = "on" ] && pass "WAL 归档开启（archive_mode=on）" || fail "archive_mode=$ARCHIVE_ON"

# 1. basebackup 基线
$PG env PGPASSWORD=dr pg_basebackup -U postgres -h localhost -D /tmp/base -Fp -X stream > /dev/null 2>&1
pass "pg_basebackup 基线（/tmp/base）"

# 2. 哨兵写入 + 记录目标时刻（恢复回卷点）
$PSQL -qc "CREATE TABLE dr_probe (id int primary key, name text); INSERT INTO dr_probe VALUES (1, 'sentinel')"
sleep 1
TARGET_TS=$(date -u +"%Y-%m-%d %H:%M:%S UTC")
$PSQL -qc "INSERT INTO dr_probe VALUES (2, 'after_target')"
pass "哨兵写入 + 目标时刻（${TARGET_TS}）"

# 3. 破坏：删哨兵 → 强制 WAL 切换归档（不切换则目标时刻 WAL 未归档——
# recovery ended before target 实证）
$PSQL -qc "DELETE FROM dr_probe WHERE id = 1"
[ "$($PSQL -tAc "SELECT count(*) FROM dr_probe WHERE id = 1")" = "0" ] && pass "破坏（哨兵删除）" || fail "破坏失败"
$PSQL -qc "SELECT pg_switch_wal()" > /dev/null
sleep 2

# 4. 恢复：停库 → base 回填 + recovery 参数（PG16）→ 回卷
docker cp "$DRC":/tmp/base /tmp/dr-base > /dev/null 2>&1
docker cp "$DRC":/var/lib/postgresql/data/pgwal /tmp/dr-pgwal > /dev/null 2>&1
docker stop "$DRC" > /dev/null 2>&1
docker run --rm -v "$DRV":/data -v /tmp/dr-base:/base -v /tmp/dr-pgwal:/wal alpine sh -c "
  rm -rf /data/* && cp -r /base/* /data/
  mkdir -p /data/pgwal && cp /wal/* /data/pgwal/ 2>/dev/null || true
  echo \"restore_command = 'cp /var/lib/postgresql/data/pgwal/%f %p'\" >> /data/postgresql.conf
  echo \"recovery_target_time = '${TARGET_TS}'\" >> /data/postgresql.conf
  echo \"recovery_target_inclusive = false\" >> /data/postgresql.conf
  chown -R 999:999 /data
  touch /data/recovery.signal
" > /dev/null 2>&1
docker start "$DRC" > /dev/null 2>&1
sleep 6
for i in $(seq 1 40); do $PSQL -tAc "SELECT 1" > /dev/null 2>&1 && break; sleep 1; done

# 5. 断言：哨兵在、目标后不在（点时间恢复语义）
SENTINEL_BACK=$($PSQL -tAc "SELECT count(*) FROM dr_probe WHERE id = 1" 2>/dev/null || echo 0)
AFTER_GONE=$($PSQL -tAc "SELECT count(*) FROM dr_probe WHERE id = 2" 2>/dev/null || echo 9)
[ "$SENTINEL_BACK" = "1" ] && pass "哨兵恢复（点时间回卷）" || fail "哨兵未恢复: $SENTINEL_BACK"
[ "$AFTER_GONE" = "0" ] && pass "目标后写入回卷消失（时间点语义）" || fail "目标后数据仍在: $AFTER_GONE"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
