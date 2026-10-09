-- 审计 A 批：效果账本（exactly-once 的第一块——Provider 接受证据/结果/usage
-- 作为预算与对账事实；「发送后 journal 前崩溃」的双计费窗口由此可对账）。
CREATE TABLE IF NOT EXISTS llm_calls (
  id            bigserial PRIMARY KEY,
  run_id        text NOT NULL,
  step          int  NOT NULL,
  dispatch_seq  int  NOT NULL DEFAULT 1,          -- 同 run+step 的第 N 次派发尝试
  state         text NOT NULL DEFAULT 'prepared', -- prepared | dispatched | result | unknown
  tokens_in     bigint NOT NULL DEFAULT 0,
  tokens_out    bigint NOT NULL DEFAULT 0,
  usage_partial boolean NOT NULL DEFAULT false,
  usage_unknown boolean NOT NULL DEFAULT false,
  err           text,
  prepared_at   timestamptz NOT NULL DEFAULT now(),
  dispatched_at timestamptz,
  result_at     timestamptz,
  UNIQUE (run_id, step)
);
CREATE INDEX IF NOT EXISTS llm_calls_run_idx ON llm_calls (run_id, step);

CREATE TABLE IF NOT EXISTS mcp_calls (
  id            bigserial PRIMARY KEY,
  run_id        text NOT NULL,
  step          int  NOT NULL,
  server        text NOT NULL,
  tool          text NOT NULL,
  state         text NOT NULL DEFAULT 'prepared', -- prepared | dispatched | result | unknown
  err           text,
  prepared_at   timestamptz NOT NULL DEFAULT now(),
  dispatched_at timestamptz,
  result_at     timestamptz,
  UNIQUE (run_id, step, server, tool)
);
CREATE INDEX IF NOT EXISTS mcp_calls_run_idx ON mcp_calls (run_id, step);
