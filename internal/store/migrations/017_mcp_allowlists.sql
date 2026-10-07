-- 期 3 §C：MCP 网关 allowlist（基础设施安全面——工具级技术权限；
-- 空 allowlist = 全拒（默认安全），非业务语义）。
CREATE TABLE IF NOT EXISTS mcp_allowlists (
  tenant_id    text NOT NULL REFERENCES orgs (id),
  server       text NOT NULL,
  tool_pattern text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, server, tool_pattern)
);
