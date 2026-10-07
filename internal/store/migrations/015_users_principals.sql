-- 期 3 §A：principal 化——users 是技术主体（归属/权限/限流），与计费解耦
-- （三层分离：tenant 隔离边界 / principal 技术主体 / 业务计费在参考业务层）。
CREATE TABLE IF NOT EXISTS users (
  id        text PRIMARY KEY,
  tenant_id text NOT NULL, -- 基础设施租户（= orgs.id；隔离边界，无业务语义）
  name      text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS users_tenant_idx ON users (tenant_id);

-- api key 绑 principal（可空 = 租户级 key，旧语义）
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS user_id text;

-- 用量 principal 归属（回填后置——run.bound 记 principal 后增量回填）
ALTER TABLE usage ADD COLUMN IF NOT EXISTS user_id text;
