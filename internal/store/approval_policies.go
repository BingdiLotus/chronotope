package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ApprovalPolicy 是审批策略行（期 3 §B 参考业务层）。
type ApprovalPolicy struct {
	TenantID     string    `json:"tenant_id"`
	ToolPatterns []string  `json:"tool_patterns"`
	Approvers    []string  `json:"approvers"`
	TTLSeconds   int64     `json:"ttl_seconds"`
	CreatedAt    time.Time `json:"created_at"`
}

// UpsertApprovalPolicy 写策略（幂等覆盖）。
func (s *Store) UpsertApprovalPolicy(ctx context.Context, p ApprovalPolicy) error {
	patterns, _ := json.Marshal(p.ToolPatterns)
	approvers, _ := json.Marshal(p.Approvers)
	const q = `
INSERT INTO org_approval_policies (tenant_id, tool_patterns, approvers, ttl_seconds)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id) DO UPDATE
SET tool_patterns = EXCLUDED.tool_patterns,
    approvers = EXCLUDED.approvers,
    ttl_seconds = EXCLUDED.ttl_seconds`
	if _, err := s.Pool.Exec(ctx, q, p.TenantID, patterns, approvers, p.TTLSeconds); err != nil {
		return fmt.Errorf("store: upsert approval policy: %w", err)
	}
	return nil
}

// GetApprovalPolicy 读策略（无 → ErrNotFound）。
func (s *Store) GetApprovalPolicy(ctx context.Context, tenantID string) (*ApprovalPolicy, error) {
	const q = `
SELECT tenant_id, tool_patterns, approvers, ttl_seconds, created_at
FROM org_approval_policies WHERE tenant_id = $1`
	var (
		p         ApprovalPolicy
		patterns  json.RawMessage
		approvers json.RawMessage
	)
	err := s.Pool.QueryRow(ctx, q, tenantID).Scan(&p.TenantID, &patterns, &approvers, &p.TTLSeconds, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get approval policy: %w", err)
	}
	_ = json.Unmarshal(patterns, &p.ToolPatterns)
	_ = json.Unmarshal(approvers, &p.Approvers)
	return &p, nil
}

// AuditEvent 是 org 级审计行（期 3 §B：按事件类型过滤的审计导出）。
type AuditEvent struct {
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	At        time.Time       `json:"at"`
}

// ListOrgAuditEvents 按租户 + 事件类型前缀读审计（join sessions 归属；
// kind 形如 "audit."）。
func (s *Store) ListOrgAuditEvents(ctx context.Context, orgID, kind string, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
SELECT e.session_id, COALESCE(e.run_id, ''), e.seq, e.type, e.payload, e.at
FROM events e JOIN sessions s ON s.id = e.session_id
WHERE s.org_id = $1 AND e.type LIKE $2
ORDER BY e.id DESC LIMIT $3`
	rows, err := s.Pool.Query(ctx, q, orgID, kind+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("store: list org audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.SessionID, &e.RunID, &e.Seq, &e.Type, &e.Payload, &e.At); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
