package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// HarnessRow 是 harness 注册行（装配设计 §2.2——与 ExecutorRow 同款）。
type HarnessRow struct {
	OrgID        string
	Name         string
	Endpoint     string
	Version      string
	Capabilities []string
	State        string
	RegisteredAt time.Time
	HeartbeatAt  time.Time
}

// RegisterHarness 注册/心跳（同 org+name+version 覆盖 heartbeat；capabilities
// 更新）。
func (s *Store) RegisterHarness(ctx context.Context, orgID, name, endpoint, version string, capabilities []string) error {
	capJSON, _ := json.Marshal(capabilities)
	const q = `
INSERT INTO harness_registry (org_id, name, endpoint, version, capabilities, state, heartbeat_at)
VALUES ($1, $2, $3, $4, $5, 'active', now())
ON CONFLICT (org_id, name, version) DO UPDATE
SET endpoint = EXCLUDED.endpoint, capabilities = EXCLUDED.capabilities, heartbeat_at = now()`
	if _, err := s.Pool.Exec(ctx, q, orgID, name, endpoint, version, capJSON); err != nil {
		return fmt.Errorf("store: register harness: %w", err)
	}
	return nil
}

// ListHarnesses org 的 harness 注册列表。
func (s *Store) ListHarnesses(ctx context.Context, orgID string) ([]HarnessRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT org_id, name, endpoint, version, capabilities::text, state, registered_at, heartbeat_at FROM harness_registry WHERE org_id = $1 ORDER BY name, version`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list harnesses: %w", err)
	}
	defer rows.Close()
	var out []HarnessRow
	for rows.Next() {
		var h HarnessRow
		var caps string
		if err := rows.Scan(&h.OrgID, &h.Name, &h.Endpoint, &h.Version, &caps, &h.State, &h.RegisteredAt, &h.HeartbeatAt); err != nil {
			return nil, fmt.Errorf("store: scan harness: %w", err)
		}
		_ = json.Unmarshal([]byte(caps), &h.Capabilities)
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetActiveHarness 解析链查询：org+name 的 active 版本（升级切换后的新 run
// 走这里；draining/retired 不返回）。
func (s *Store) GetActiveHarness(ctx context.Context, orgID, name string) (*HarnessRow, error) {
	const q = `SELECT org_id, name, endpoint, version, capabilities::text, state, registered_at, heartbeat_at FROM harness_registry WHERE org_id = $1 AND name = $2 AND state = 'active' ORDER BY version DESC LIMIT 1`
	var h HarnessRow
	var caps string
	if err := s.Pool.QueryRow(ctx, q, orgID, name).Scan(&h.OrgID, &h.Name, &h.Endpoint, &h.Version, &caps, &h.State, &h.RegisteredAt, &h.HeartbeatAt); err != nil {
		return nil, fmt.Errorf("store: active harness: %w", err)
	}
	_ = json.Unmarshal([]byte(caps), &h.Capabilities)
	return &h, nil
}

// SetHarnessState 切 active/draining/retire（升级生命周期——切换后旧 run
// 沿旧快照、新 run 走新 active）。
func (s *Store) SetHarnessState(ctx context.Context, orgID, name, version, state string) error {
	const q = `UPDATE harness_registry SET state = $4 WHERE org_id = $1 AND name = $2 AND version = $3`
	tag, err := s.Pool.Exec(ctx, q, orgID, name, version, state)
	if err != nil {
		return fmt.Errorf("store: set harness state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
