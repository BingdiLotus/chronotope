-- ComputeLease（正确性二期 ⑨）：沙箱租约账本——run 活着（含挂起）经每步
-- 检查点续约持有；GC 回收条件 = TTL 过期且无未过期租约（依赖安全释放）；
-- generation 单调递增（防旧持有者误释放新租约——fencing 第一块积木）。
CREATE TABLE IF NOT EXISTS sandbox_leases (
  sandbox_id text PRIMARY KEY REFERENCES sandboxes (sandbox_id),
  run_id     text NOT NULL,
  generation bigint NOT NULL DEFAULT 1,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sandbox_leases_run_idx ON sandbox_leases (run_id);
