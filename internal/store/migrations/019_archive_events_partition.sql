-- 期 4 §A：冷层分区表（回热物化预留）——archive_events 按月 RANGE 分区。
--
-- 为什么热表 events 不分区（实证结论，防止未来重蹈）：
-- PostgreSQL 分区表的唯一约束/主键必须包含分区键；events 依赖两个全局唯一
-- 语义——主键 id 与 dedupe_key 全局去重（AppendEvent 的 ON CONFLICT 幂等）。
-- 若按 at 分区，重放同 dedupe_key 事件时 at=now() 不同 → (dedupe_key, at)
-- 唯一失效 → 重放去重被破坏（幂等语义是正确性地基，不可妥协）。
-- 因此分层策略：热表 events 保持单表（dedupe 语义完整）；冷数据经归档
-- （ndjson.gz → RustFS）+ 本表按月分区承接回热物化（归档数据只追加、无
-- 去重约束——分区限制不冲突）。
CREATE TABLE IF NOT EXISTS archive_events (
  id         bigserial,
  session_id text NOT NULL,
  run_id     text NOT NULL,
  seq        bigint NOT NULL,
  type       text NOT NULL,
  payload    jsonb NOT NULL,
  at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id, at) -- 分区表要求：主键含分区键
) PARTITION BY RANGE (at);

-- 首月分区 + 默认分区（未覆盖月份兜底——归档脚本按需建未来分区）
CREATE TABLE IF NOT EXISTS archive_events_2026_10 PARTITION OF archive_events
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE IF NOT EXISTS archive_events_default PARTITION OF archive_events DEFAULT;
