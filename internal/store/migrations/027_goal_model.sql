-- M2 对象模型：Goal（慢变量）/ WorkItem（可领取切片）/ Evidence（新鲜度）
-- 与层 0 的 ActionClaim（动作级可过期占有权）。
CREATE TABLE IF NOT EXISTS goals (
  id          text PRIMARY KEY,
  org_id      text NOT NULL REFERENCES orgs (id),
  objective   text NOT NULL,
  scope       text NOT NULL DEFAULT '',
  version     int  NOT NULL DEFAULT 1,
  owner       text NOT NULL DEFAULT '',
  status      text NOT NULL DEFAULT 'active', -- active | paused | done
  state_hash  text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS goals_org_idx ON goals (org_id);

CREATE TABLE IF NOT EXISTS work_items (
  id           text PRIMARY KEY,
  goal_id      text NOT NULL REFERENCES goals (id),
  description  text NOT NULL,
  priority     int  NOT NULL DEFAULT 0,
  dependencies text NOT NULL DEFAULT '',
  task_class   text NOT NULL DEFAULT '',
  status       text NOT NULL DEFAULT 'open', -- open | claimed | done
  evidence_ref text NOT NULL DEFAULT '',
  claimed_by   text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS work_items_goal_idx ON work_items (goal_id, status);

CREATE TABLE IF NOT EXISTS evidence (
  id              text PRIMARY KEY,
  session_id      text NOT NULL REFERENCES sessions (id),
  run_id          text NOT NULL DEFAULT '',
  blob_hash       text NOT NULL,
  produced_at     timestamptz NOT NULL DEFAULT now(),
  valid_for       text NOT NULL DEFAULT '',
  source_revision text NOT NULL DEFAULT '',
  expires_at      timestamptz,
  method          text NOT NULL DEFAULT '',
  state_hash      text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS evidence_session_idx ON evidence (session_id);

CREATE TABLE IF NOT EXISTS action_claims (
  scope        text PRIMARY KEY,           -- 动作域（如 repo:branch_x）
  agent_id     text NOT NULL,
  capabilities text NOT NULL DEFAULT '',   -- 能力清单（逗号分隔）
  generation   bigint NOT NULL DEFAULT 1,
  expires_at   timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
