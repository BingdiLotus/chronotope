-- Harness 装配阶段 1：org 级 harness 注册表（与 executor 注册表同款——
-- 期 4 §B 的多宿主装配模式复用）。
CREATE TABLE IF NOT EXISTS harness_registry (
  org_id        text NOT NULL,
  name          text NOT NULL,
  endpoint      text NOT NULL,
  version       text NOT NULL,
  capabilities  jsonb NOT NULL DEFAULT '[]',
  state         text NOT NULL DEFAULT 'active', -- active | draining | retired
  registered_at timestamptz NOT NULL DEFAULT now(),
  heartbeat_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, name, version)
);
CREATE INDEX IF NOT EXISTS harness_registry_active_idx ON harness_registry (org_id, name, state);
