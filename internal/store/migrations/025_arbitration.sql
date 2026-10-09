-- 仲裁合同深化（审计 4.1 合同表）：request hash 冻结 + 冻结结果引用。
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS request_hash text;
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS result jsonb;
ALTER TABLE mcp_calls ADD COLUMN IF NOT EXISTS request_hash text;
ALTER TABLE mcp_calls ADD COLUMN IF NOT EXISTS result jsonb;
ALTER TABLE mcp_calls ADD COLUMN IF NOT EXISTS call_key text; -- ToolCall.ID/参数 digest（同一 step 多工具调用的身份）
DROP INDEX IF EXISTS mcp_calls_run_idx;
CREATE UNIQUE INDEX IF NOT EXISTS mcp_calls_identity_idx ON mcp_calls (run_id, step, server, tool, call_key);
CREATE INDEX IF NOT EXISTS mcp_calls_run_idx ON mcp_calls (run_id, step);
