-- 期 3 §B：审批策略（参考业务层数据——ApprovalRouter 策略缝的参考实现；
-- 无行 = ManualOnly 人工审批，基础设施默认）。
CREATE TABLE IF NOT EXISTS org_approval_policies (
  tenant_id  text PRIMARY KEY REFERENCES orgs (id),
  tool_patterns jsonb NOT NULL DEFAULT '[]', -- 空 = 全部 class 2
  approvers  jsonb NOT NULL DEFAULT '[]',    -- 审批人集合
  ttl_seconds bigint NOT NULL DEFAULT 86400  -- 过期自动拒绝（默认 24h）
);
