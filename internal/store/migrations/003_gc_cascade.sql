-- W8 孤儿 GC：沙箱删除时级联清理 exec 幂等缓存行（缓存随沙箱生命周期）
-- 前置清理：008 解耦级联后，删沙箱不再清理缓存行——重放 003 重建 FK 时
-- 孤儿行（引用已删沙箱）会违反约束（store_test 实证）——先清后建。
DELETE FROM sandbox_execs e
WHERE NOT EXISTS (SELECT 1 FROM sandboxes s WHERE s.sandbox_id = e.sandbox_id);

ALTER TABLE sandbox_execs DROP CONSTRAINT IF EXISTS sandbox_execs_sandbox_id_fkey;
ALTER TABLE sandbox_execs ADD CONSTRAINT sandbox_execs_sandbox_id_fkey
  FOREIGN KEY (sandbox_id) REFERENCES sandboxes (sandbox_id) ON DELETE CASCADE;
