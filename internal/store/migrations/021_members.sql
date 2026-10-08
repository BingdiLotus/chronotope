-- 期 5 §A：org 成员表（角色模型——层 2 参考业务：org_admin/member/auditor）。
CREATE TABLE IF NOT EXISTS members (
  org_id     text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       text NOT NULL DEFAULT 'member',  -- org_admin | member | auditor
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS members_org_idx ON members (org_id);
