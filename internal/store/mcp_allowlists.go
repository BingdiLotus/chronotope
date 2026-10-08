package store

import (
	"context"
	"fmt"
	"strings"
)

// UpsertMCPAllowlist 覆盖写租户的 server 工具白名单（空列表 = 全拒，默认安全）。
func (s *Store) UpsertMCPAllowlist(ctx context.Context, tenantID, server string, patterns []string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: allowlist begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM mcp_allowlists WHERE tenant_id = $1 AND server = $2`, tenantID, server); err != nil {
		return fmt.Errorf("store: allowlist clear: %w", err)
	}
	for _, p := range patterns {
		if _, err := tx.Exec(ctx,
			`INSERT INTO mcp_allowlists (tenant_id, server, tool_pattern) VALUES ($1, $2, $3)`,
			tenantID, server, p); err != nil {
			return fmt.Errorf("store: allowlist insert: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// MCPToolAllowed 判定工具是否在租户白名单内（前缀匹配；无行 = 拒绝）。
// HasMCPAllowlist 判 org 是否对 server 有显式 allowlist 配置（执行层防御
// 的开关——无配置 = 旧 MCP 路径放行（W6 历史段兼容）；有配置则按匹配拦）。
func (s *Store) HasMCPAllowlist(ctx context.Context, tenantID, server string) (bool, error) {
	const q = `SELECT EXISTS (SELECT 1 FROM mcp_allowlists WHERE tenant_id = $1 AND server = $2)`
	var has bool
	if err := s.Pool.QueryRow(ctx, q, tenantID, server).Scan(&has); err != nil {
		return false, fmt.Errorf("store: has allowlist: %w", err)
	}
	return has, nil
}

func (s *Store) MCPToolAllowed(ctx context.Context, tenantID, server, tool string) (bool, error) {
	const q = `SELECT tool_pattern FROM mcp_allowlists WHERE tenant_id = $1 AND server = $2`
	rows, err := s.Pool.Query(ctx, q, tenantID, server)
	if err != nil {
		return false, fmt.Errorf("store: allowlist query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pat string
		if err := rows.Scan(&pat); err != nil {
			return false, err
		}
		if strings.HasPrefix(tool, pat) {
			return true, nil
		}
	}
	return false, rows.Err()
}
