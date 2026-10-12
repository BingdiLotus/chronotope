package store

import (
	"context"
	"fmt"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/credential"
)

// CredentialRow 是凭据注册行（期 7 下沉：credential.Row 别名）。
type CredentialRow = credential.Row

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
	return s.RecordCredentialEvent(ctx, orgID, id, ref, "set")
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

// RecordCredentialEvent 凭据变更事件（set/revoke/unset——审计与轮换的
// 可观测；dsh 的 reference-updated 对应）。
func (s *Store) RecordCredentialEvent(ctx context.Context, orgID, credentialID, ref, action string) error {
	const q = `INSERT INTO credential_events (org_id, credential_id, ref, action) VALUES ($1, $2, $3, $4)`
	if _, err := s.Pool.Exec(ctx, q, orgID, credentialID, ref, action); err != nil {
		return fmt.Errorf("store: credential event: %w", err)
	}
	return nil
}

// ListCredentialEvents org 的凭据变更事件流（按时间序）。
func (s *Store) ListCredentialEvents(ctx context.Context, orgID string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT credential_id, ref, action, at FROM credential_events WHERE org_id = $1 ORDER BY id DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list credential events: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var cid, ref, action string
		var at time.Time
		if err := rows.Scan(&cid, &ref, &action, &at); err != nil {
			return nil, fmt.Errorf("store: scan credential event: %w", err)
		}
		out = append(out, map[string]any{"credential_id": cid, "ref": ref, "action": action, "at": at})
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
	var orgID, ref string
	if rErr := s.Pool.QueryRow(ctx, `SELECT org_id, ref FROM credentials WHERE id = $1`, id).Scan(&orgID, &ref); rErr == nil {
		_ = s.RecordCredentialEvent(ctx, orgID, id, ref, "revoke")
	}
	return nil
}
