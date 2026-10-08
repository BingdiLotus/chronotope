package store

import (
	"context"
	"fmt"
	"time"
)

// Member 是 org 成员行（期 5 §A 角色模型——层 2 参考业务）。
type Member struct {
	OrgID     string    `json:"org_id"`
	UserID    string    `json:"user_id"`
	Role      string    `json:"role"` // org_admin | member | auditor
	CreatedAt time.Time `json:"created_at"`
}

// ValidRole 角色白名单（层 2 参考业务——业务方可替换）。
func ValidRole(role string) bool {
	switch role {
	case "org_admin", "member", "auditor":
		return true
	}
	return false
}

// AddMember 幂等添加成员（同 (org,user) 覆盖角色——管理 API 直建，无邮件流）。
func (s *Store) AddMember(ctx context.Context, m Member) error {
	const q = `
INSERT INTO members (org_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`
	if _, err := s.Pool.Exec(ctx, q, m.OrgID, m.UserID, m.Role); err != nil {
		return fmt.Errorf("store: add member: %w", err)
	}
	return nil
}

// ListMembers 列 org 成员（角色排序：admin 在前）。
func (s *Store) ListMembers(ctx context.Context, orgID string) ([]Member, error) {
	const q = `
SELECT org_id, user_id, role, created_at FROM members
WHERE org_id = $1
ORDER BY CASE role WHEN 'org_admin' THEN 0 WHEN 'member' THEN 1 ELSE 2 END, created_at`
	rows, err := s.Pool.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list members: %w", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.OrgID, &m.UserID, &m.Role, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RemoveMember 移除成员。
func (s *Store) RemoveMember(ctx context.Context, orgID, userID string) error {
	const q = `DELETE FROM members WHERE org_id = $1 AND user_id = $2`
	if _, err := s.Pool.Exec(ctx, q, orgID, userID); err != nil {
		return fmt.Errorf("store: remove member: %w", err)
	}
	return nil
}
