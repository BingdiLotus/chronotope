-- 承诺原语（修正版 §2.1）：agent 对外义务的一等实体——事件为真相、
-- 投影表为查询面。状态机七态：pending/open/fulfilled/broken/expired/
-- canceled/orphaned（§2.2 的 INTENT 态：已记录与已确认发出分离）。
CREATE TABLE IF NOT EXISTS agent_commitments (
  id             text PRIMARY KEY,        -- 承诺 ID（幂等键）
  agent_id       text NOT NULL REFERENCES agents(id),

  -- 可追溯性
  made_in_thread text NOT NULL,           -- 在哪个线程说的
  made_in_run    text REFERENCES runs(id),
  made_at        timestamptz NOT NULL,

  -- 对象
  counterparty   text,                    -- person_id（多平台后置）

  -- 内容
  speech_act     text NOT NULL,           -- commissive / assertive / directive
  content        text NOT NULL,
  content_digest text NOT NULL,           -- sha256(content)——字节级判重
  decider_confidence numeric,             -- 准入门的校准概率（审计——群体口径）

  -- 时间约束
  due_at         timestamptz,

  -- 状态（由事件派生）
  state          text NOT NULL DEFAULT 'pending',  -- 见 §2.2 七态
  resolved_at    timestamptz,
  confirmed_at   timestamptz,             -- 发送回执时刻（pending→open）

  -- 可见性
  visibility     text NOT NULL DEFAULT 'agent'   -- agent（跨平台） / thread

);

CREATE INDEX IF NOT EXISTS idx_agent_commitments_open ON agent_commitments (agent_id) WHERE state = 'open';
CREATE INDEX IF NOT EXISTS idx_agent_commitments_due ON agent_commitments (due_at) WHERE state = 'open' AND due_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_commitments_pending ON agent_commitments (made_at) WHERE state = 'pending';
