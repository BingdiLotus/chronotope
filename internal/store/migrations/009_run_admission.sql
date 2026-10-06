-- 接纳屏障辅助（评审 #6）：无 schema 变更——queued 重投扫描依赖 runs 行
-- （queued 状态 + created_at 年龄），本迁移仅加索引加速扫描。
CREATE INDEX IF NOT EXISTS runs_queued_age_idx ON runs (created_at) WHERE status = 'queued';
