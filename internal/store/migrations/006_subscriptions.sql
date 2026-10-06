-- 事件投递订阅（outbox 事件投递，落地方案 §5）：会话级订阅，事件同事务入队，
-- 投递 worker 定时拉取 + 指数退避。channel: webhook（URL POST，兼容
-- Slack/Feishu incoming webhook 形态）| email（SMTP）。
CREATE TABLE IF NOT EXISTS subscriptions (
  id         bigserial PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  channel    text NOT NULL DEFAULT 'webhook',
  target     text NOT NULL,               -- webhook: URL；email: 收件地址
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS subscriptions_session_idx ON subscriptions (session_id);
