-- 交付清单（W8 后置「通知与交付」）：run 完成时 worker 写入交付物清单，
-- 投递方（邮件/IM）轮询未投递行、投递后回执标记。
-- 命名 deliverables 以区分 001 的事件投递 outbox（url/attempts 重试表，后置未用）。
CREATE TABLE IF NOT EXISTS deliverables (
  id           bigserial PRIMARY KEY,
  run_id       text NOT NULL UNIQUE,      -- 每 run 一行（journal 重放幂等）
  session_id   text NOT NULL REFERENCES sessions(id),
  kind         text NOT NULL DEFAULT 'run_completed',
  payload      jsonb NOT NULL,            -- 交付物清单（final/steps/tokens/产物）
  delivered_at timestamptz,               -- NULL = 未投递
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deliverables_session_idx ON deliverables (session_id, id);
CREATE INDEX IF NOT EXISTS deliverables_undelivered_idx ON deliverables (delivered_at) WHERE delivered_at IS NULL;
