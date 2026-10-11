-- A4（能力适配矩阵）：session 的 steering 队列——Pi 的 follow-up
-- inbox/steering 对应（运行中的输入排队——run 的 drain 点注入）。
CREATE TABLE IF NOT EXISTS session_steers (
  id          bigserial PRIMARY KEY,
  session_id  text NOT NULL REFERENCES sessions(id),
  input       text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  consumed_at timestamptz
);
CREATE INDEX IF NOT EXISTS session_steers_pending_idx ON session_steers (session_id, id) WHERE consumed_at IS NULL;
