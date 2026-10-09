-- 仲裁合同深化（审计 4.1 合同表）：request hash 冻结 + 冻结结果引用。
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS request_hash text;
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS result jsonb;
ALTER TABLE mcp_calls ADD COLUMN IF NOT EXISTS request_hash text;
ALTER TABLE mcp_calls ADD COLUMN IF NOT EXISTS result jsonb;
