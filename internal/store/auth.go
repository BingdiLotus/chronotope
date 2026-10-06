package store

import (
	"context"
	"fmt"
	"time"
)

// APIKeyRow 是 api_keys 行（key 只存 sha256 哈希，明文仅生成时返回一次）。
type APIKeyRow struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	KeyHash   string    `json:"-"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateAPIKey 写入 key 哈希（幂等：hash 冲突返回已存在行）。
func (s *Store) CreateAPIKey(ctx context.Context, id, orgID, keyHash string, scopes []string) error {
	const q = `
INSERT INTO api_keys (id, org_id, key_hash, scopes)
VALUES ($1, $2, $3, $4)
ON CONFLICT (key_hash) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, id, orgID, keyHash, scopes); err != nil {
		return fmt.Errorf("store: create api key: %w", err)
	}
	return nil
}

// GetAPIKeyByHash 按 key 哈希查行（认证中间件）。
func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyRow, error) {
	const q = `
SELECT id, org_id, key_hash, scopes, created_at
FROM api_keys WHERE key_hash = $1`
	var row APIKeyRow
	if err := s.Pool.QueryRow(ctx, q, keyHash).Scan(&row.ID, &row.OrgID, &row.KeyHash, &row.Scopes, &row.CreatedAt); err != nil {
		if isNoRowsErr(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get api key: %w", err)
	}
	return &row, nil
}
