-- A2（能力适配矩阵）：mcp_calls 的 duration_ms——工具执行的时序维度
-- （Pi 的 tool_execution_end.durationMs 对应；账本的时序对账）。
ALTER TABLE mcp_calls ADD COLUMN IF NOT EXISTS duration_ms bigint;
