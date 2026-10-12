-- 凭据服务阶段 ②：credentials 表（org 级注册——backend 指针 + 状态；
-- 值不进此表（env 后端）/外部指针（external 后端））。
CREATE TABLE IF NOT EXISTS credentials (
  id          text PRIMARY KEY,
  org_id      text NOT NULL,
  ref         text NOT NULL, -- 引用名（POSIX——CredentialRef）
  backend     text NOT NULL DEFAULT 'env', -- env | external
  backend_ref text NOT NULL DEFAULT '',    -- external 的指针（不存值）
  state       text NOT NULL DEFAULT 'active', -- active | revoked
  rotated_at  timestamptz NOT NULL DEFAULT now(),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS credentials_org_idx ON credentials (org_id, state);
