package store

import (
	"context"
	"fmt"
	"time"
)

// CredentialRow 是凭据注册行（阶段 ②——org 级；值不进此行）。
type CredentialRow struct {
	ID         string
	OrgID      string
	Ref        string
	Backend    string
	BackendRef string
	State      string
	RotatedAt  time.Time
}

// RegisterCredential 注册凭据引用（同 id 覆盖；值永不经此方法）。
func (s *Store) RegisterCredential(ctx context.Context, id, orgID, ref, backend, backendRef string) error {
	const q = `
INSERT INTO credentials (id, org_id, ref, backend, backend_ref, state)
VALUES ($1, $2, $3, $4, $5, 'active')
ON CONFLICT (id) DO UPDATE
SET ref = EXCLUDED.ref, backend = EXCLUDED.backend, backend_ref = EXCLUDED.backend_ref,
    state = 'active', rotated_at = now()`
	if _, err := s.Pool.Exec(ctx, q, id, orgID, ref, backend, backendRef); err != nil {
		return fmt.Errorf("store: register credential: %w", err)
	}
	return nil
}

// ListCredentials org 的凭据注册（describe 视图——无值槽）。
func (s *Store) ListCredentials(ctx context.Context, orgID string) ([]CredentialRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, org_id, ref, backend, backend_ref, state, rotated_at FROM credentials WHERE org_id = $1 ORDER BY ref`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	defer rows.Close()
	var out []CredentialRow
	for rows.Next() {
		var c CredentialRow
		if err := rows.Scan(&c.ID, &c.OrgID, &c.Ref, &c.Backend, &c.BackendRef, &c.State, &c.RotatedAt); err != nil {
			return nil, fmt.Errorf("store: scan credential: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeCredential 吊销（active → revoked——解析返回未配置；显式失败不
// 静默换凭据）。
func (s *Store) RevokeCredential(ctx context.Context, id string) error {
	const q = `UPDATE credentials SET state = 'revoked', rotated_at = now() WHERE id = $1 AND state = 'active'`
	tag, err := s.Pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: revoke credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
