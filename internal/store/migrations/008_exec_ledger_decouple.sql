-- effect 账本与沙箱生命周期解耦（评审 #3）：exec 缓存行不再随沙箱删除级联清理——
-- 业务 effect identity 必须跨 sandbox 生命周期保留（「恢复先查询原操作」的依据）；
-- 缓存行独立按自身 expires_at 过期（30 天，原 24h 对跨周恢复太短）。
ALTER TABLE sandbox_execs DROP CONSTRAINT IF EXISTS sandbox_execs_sandbox_id_fkey;
ALTER TABLE sandbox_execs ALTER COLUMN sandbox_id DROP NOT NULL;
UPDATE sandbox_execs SET expires_at = now() + interval '30 days' WHERE state = 'done';
