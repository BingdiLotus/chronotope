-- 期 6 ①：WaitFor 收敛 + yield intent 捕获——「现在在等什么、为什么停」的一等
-- 对象（DECIDE 从启发式升级为可判定的地基）。
CREATE TABLE IF NOT EXISTS session_waits (
  session_id    text NOT NULL REFERENCES sessions (id),
  kind          text NOT NULL, -- timer | task_result | external | operator_input
  handle        text NOT NULL, -- WaitHandle（awakeable id / schedule key）
  intent        text NOT NULL DEFAULT '', -- 为什么停（假设了什么）
  expect        text NOT NULL DEFAULT '', -- 期待的返回条件（什么算满足）
  registered_at timestamptz NOT NULL DEFAULT now(),
  resolved_at   timestamptz,
  PRIMARY KEY (session_id, kind, handle)
);
CREATE INDEX IF NOT EXISTS session_waits_active_idx ON session_waits (session_id, resolved_at);
