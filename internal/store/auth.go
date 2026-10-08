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
	UserID    string    `json:"user_id"` // principal（期 3 §A；空 = 租户级 key，旧语义）
	KeyHash   string    `json:"-"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
}

// User 是技术主体（principal——归属/权限/限流；与计费解耦，期 3 §A）。
type User struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateUser 幂等建主体（同 id 冲突忽略）。
func (s *Store) CreateUser(ctx context.Context, u User) error {
	const q = `
INSERT INTO users (id, tenant_id, name)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, u.ID, u.OrgID, u.Name); err != nil {
		return fmt.Errorf("store: create user: %w", err)
	}
	return nil
}

// CreateAPIKey 写入 key 哈希（幂等：hash 冲突返回已存在行）。
// CreateAPIKey 建租户级 key（user_id 空——产品语义面：org 级 key 与 principal
// key 并存；曾误删——后续任务（引导/供给工具）的自然入口）。
func (s *Store) CreateAPIKey(ctx context.Context, id, orgID, keyHash string, scopes []string) error {
	return s.CreateAPIKeyForUser(ctx, id, orgID, "", keyHash, scopes)
}

// CreateAPIKeyForUser 建 key 绑 principal（空 userID = 租户级 key）。
func (s *Store) CreateAPIKeyForUser(ctx context.Context, id, orgID, userID, keyHash string, scopes []string) error {
	const q = `
INSERT INTO api_keys (id, org_id, user_id, key_hash, scopes)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (key_hash) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, id, orgID, userID, keyHash, scopes); err != nil {
		return fmt.Errorf("store: create api key: %w", err)
	}
	return nil
}

// GetAPIKeyByHash 按 key 哈希查行（认证中间件）。
func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyRow, error) {
	const q = `
SELECT id, org_id, COALESCE(user_id, ''), key_hash, scopes, created_at
FROM api_keys WHERE key_hash = $1`
	var row APIKeyRow
	if err := s.Pool.QueryRow(ctx, q, keyHash).Scan(&row.ID, &row.OrgID, &row.UserID, &row.KeyHash, &row.Scopes, &row.CreatedAt); err != nil {
		if isNoRowsErr(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get api key: %w", err)
	}
	return &row, nil
}
