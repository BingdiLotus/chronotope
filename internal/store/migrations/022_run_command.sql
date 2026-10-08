-- 审计 #3/#4：接纳事务——runs 存不可变 command（input/topic——重投不丢输入）
-- + active 唯一约束（并发双 Run 的 DB 兜底——检查后 CreateRun 的竞态窗口）。
ALTER TABLE runs ADD COLUMN IF NOT EXISTS input text;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS topic text;
CREATE UNIQUE INDEX IF NOT EXISTS runs_active_session_idx ON runs (session_id)
  WHERE status IN ('queued', 'running');
