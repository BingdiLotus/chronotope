-- 执行后写窗口闭合（评审 #1）：exec 缓存加状态机与输入摘要——
-- prepared（执行前 claim，防同键并发双执行）/ done；input_digest 校验同键同输入。
-- 未知窗口（prepared 过期仍未 done）= 命令可能已执行：按 prepared_at 新鲜度
-- 判断 in-flight（拒绝重试）或 unknown（重跑覆盖）。
ALTER TABLE sandbox_execs
  ADD COLUMN IF NOT EXISTS state        text NOT NULL DEFAULT 'done',
  ADD COLUMN IF NOT EXISTS input_digest text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS prepared_at  timestamptz;
