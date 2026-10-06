-- W8 孤儿 GC：沙箱删除时级联清理 exec 幂等缓存行（缓存随沙箱生命周期）
ALTER TABLE sandbox_execs DROP CONSTRAINT IF EXISTS sandbox_execs_sandbox_id_fkey;
ALTER TABLE sandbox_execs ADD CONSTRAINT sandbox_execs_sandbox_id_fkey
  FOREIGN KEY (sandbox_id) REFERENCES sandboxes (sandbox_id) ON DELETE CASCADE;
