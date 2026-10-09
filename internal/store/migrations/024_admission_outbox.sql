-- 审计 B1：唯一接纳 outbox——持久派发状态（admission 与派发的合同：
-- pending 行被扫描重投——替换「queued/running 超时扫描」的近似机制）。
CREATE TABLE IF NOT EXISTS admission_outbox (
  run_id    text PRIMARY KEY,
  state     text NOT NULL DEFAULT 'pending', -- pending | dispatched | confirmed
  created_at timestamptz NOT NULL DEFAULT now(),
  dispatched_at timestamptz
);
