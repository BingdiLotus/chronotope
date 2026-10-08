-- 期 4 §B：多宿主 executor 池——注册表心跳 + 沙箱归属。
ALTER TABLE executors ADD COLUMN IF NOT EXISTS heartbeat_at timestamptz;
ALTER TABLE executors ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS executor_id text; -- 沙箱归属（操作路由到属主 executor）
-- 平台级 executor 无租户归属（多宿主池是平台基础设施——001 的 org 归属过时）
ALTER TABLE executors ALTER COLUMN org_id DROP NOT NULL;
ALTER TABLE executors DROP CONSTRAINT IF EXISTS executors_org_id_fkey;
