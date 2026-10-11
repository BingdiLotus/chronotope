-- B2（能力适配矩阵）：扩展层——tools/prompts/hooks 的可装配包（org 级
-- 注册 + agent 绑定——Claude skills/plugins 与 dsh 插件树的最小公约数）。
CREATE TABLE IF NOT EXISTS extensions (
  id          text PRIMARY KEY,
  org_id      text NOT NULL,
  name        text NOT NULL,
  kind        text NOT NULL DEFAULT 'tools', -- tools | prompts | hooks
  definition  jsonb NOT NULL DEFAULT '{}',
  version     text NOT NULL,
  state       text NOT NULL DEFAULT 'active', -- active | retired
  registered_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS extensions_org_idx ON extensions (org_id, kind, state);
