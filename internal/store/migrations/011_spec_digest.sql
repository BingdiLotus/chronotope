-- accepted spec digest（正确性二期 ⑩）：agent 配置的规范哈希（canonical JSON +
-- sha256）——run 绑定与审计关联；agent 升级（version+1）不改变旧 run 的
-- 绑定快照（旧 run 永远用其启动时的 config 跑完）。
ALTER TABLE agents ADD COLUMN IF NOT EXISTS spec_digest text NOT NULL DEFAULT '';
