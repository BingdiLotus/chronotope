-- 时间旅行（正式版架构 期 2）：checkpoint 树 + 分支血缘。
-- checkpoints：会话时间坐标（seq 指针 + 沙箱快照引用）——checkpoint(session, seq)
-- 把「时间面」（事件轴）与「空间面」（沙箱快照）钉在一起。
CREATE TABLE IF NOT EXISTS checkpoints (
  id           text PRIMARY KEY,
  session_id   text NOT NULL REFERENCES sessions (id),
  seq          bigint NOT NULL,
  snapshot_ref text NOT NULL DEFAULT '',
  max_message_id bigint NOT NULL DEFAULT 0,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS checkpoints_session_idx ON checkpoints (session_id, seq);
-- 幂等加列（已有库重放：早版本 012 无消息水位列）
ALTER TABLE checkpoints ADD COLUMN IF NOT EXISTS max_message_id bigint NOT NULL DEFAULT 0;

-- 分支血缘（fork 的父指针；写时复制：fork 复制事件/消息前缀）
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS forked_from_session text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS forked_at_seq bigint NOT NULL DEFAULT 0;
