package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SandboxRow 是 sandboxes 表行（契约规范 §4：沙箱事实状态在 PG，不随进程死）。
type SandboxRow struct {
	SandboxID     string
	OrgID         string
	SessionID     string
	Driver        string
	ContainerRef  *string
	Image         string
	Limits        map[string]string
	Tier          int
	SnapshotRef   *string
	FileSyncState string
	Status        string
	TTL           *time.Duration
	CreatedAt     time.Time
}

// UpsertSandbox 记录/更新沙箱事实状态。
func (s *Store) UpsertSandbox(ctx context.Context, sb *SandboxRow) error {
	limitsJSON, err := json.Marshal(sb.Limits)
	if err != nil {
		return fmt.Errorf("store: marshal limits: %w", err)
	}
	const q = `
INSERT INTO sandboxes (sandbox_id, org_id, session_id, driver, container_ref, image, limits, tier, snapshot_ref, file_sync_state, ttl, status, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
ON CONFLICT (sandbox_id) DO UPDATE SET
  container_ref = EXCLUDED.container_ref, tier = EXCLUDED.tier,
  snapshot_ref = EXCLUDED.snapshot_ref, file_sync_state = EXCLUDED.file_sync_state,
  status = EXCLUDED.status`
	if _, err := s.Pool.Exec(ctx, q,
		sb.SandboxID, sb.OrgID, sb.SessionID, sb.Driver, sb.ContainerRef, sb.Image,
		limitsJSON, sb.Tier, sb.SnapshotRef, sb.FileSyncState, sb.TTL, sb.Status); err != nil {
		return fmt.Errorf("store: upsert sandbox: %w", err)
	}
	return nil
}

// GetSandbox 读取沙箱事实状态。
func (s *Store) GetSandbox(ctx context.Context, sandboxID string) (*SandboxRow, error) {
	const q = `
SELECT sandbox_id, org_id, session_id, driver, container_ref, image, limits, tier,
       snapshot_ref, file_sync_state, ttl, status, created_at
FROM sandboxes WHERE sandbox_id = $1`
	var (
		sb         SandboxRow
		limitsJSON json.RawMessage
		ttl        pgtype.Interval
	)
	err := s.Pool.QueryRow(ctx, q, sandboxID).Scan(
		&sb.SandboxID, &sb.OrgID, &sb.SessionID, &sb.Driver, &sb.ContainerRef, &sb.Image,
		&limitsJSON, &sb.Tier, &sb.SnapshotRef, &sb.FileSyncState, &ttl, &sb.Status, &sb.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get sandbox: %w", err)
	}
	_ = json.Unmarshal(limitsJSON, &sb.Limits)
	if ttl.Valid {
		d := time.Duration(ttl.Microseconds+int64(ttl.Days)*24*3600*1e6) * time.Microsecond
		sb.TTL = &d
	}
	return &sb, nil
}

// UpdateSandboxStatus 迁移沙箱状态（creating→ready→frozen→snapshotted→destroyed）。
func (s *Store) UpdateSandboxStatus(ctx context.Context, sandboxID, status string) error {
	const q = `UPDATE sandboxes SET status = $2 WHERE sandbox_id = $1`
	if _, err := s.Pool.Exec(ctx, q, sandboxID, status); err != nil {
		return fmt.Errorf("store: update sandbox status: %w", err)
	}
	return nil
}

// UpdateSandboxTier 迁移沙箱 tier（0 运行 / 1 冻结 / 2 快照 / 3 拆除）。
func (s *Store) UpdateSandboxTier(ctx context.Context, sandboxID string, tier int, snapshotRef *string) error {
	const q = `UPDATE sandboxes SET tier = $2, snapshot_ref = COALESCE($3, snapshot_ref) WHERE sandbox_id = $1`
	if _, err := s.Pool.Exec(ctx, q, sandboxID, tier, snapshotRef); err != nil {
		return fmt.Errorf("store: update sandbox tier: %w", err)
	}
	return nil
}

// ExecRow 是 sandbox_execs 表行（幂等缓存：exec 已执行但响应丢失 → 同键重发返回缓存结果，
// 绝不复跑命令——契约规范 §4；TTL 24h；结果 >256KB 外置）。
type ExecRow struct {
	IdempotencyKey string
	SandboxID      string
	Result         json.RawMessage
	ExpiresAt      time.Time
}

// GetExec 读取幂等缓存；未命中返回 ErrNotFound。
func (s *Store) GetExec(ctx context.Context, idempotencyKey string) (*ExecRow, error) {
	const q = `SELECT idempotency_key, sandbox_id, result, expires_at FROM sandbox_execs WHERE idempotency_key = $1`
	var e ExecRow
	err := s.Pool.QueryRow(ctx, q, idempotencyKey).Scan(&e.IdempotencyKey, &e.SandboxID, &e.Result, &e.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get exec: %w", err)
	}
	if time.Now().After(e.ExpiresAt) {
		return nil, ErrNotFound // 缓存过期视为未执行（TTL 24h）
	}
	return &e, nil
}

// PutExec 写入幂等缓存（TTL 24h，契约规范 §4）。
func (s *Store) PutExec(ctx context.Context, idempotencyKey, sandboxID string, result json.RawMessage) error {
	const q = `
INSERT INTO sandbox_execs (idempotency_key, sandbox_id, result, expires_at)
VALUES ($1, $2, $3, now() + interval '24 hours')`
	if _, err := s.Pool.Exec(ctx, q, idempotencyKey, sandboxID, result); err != nil {
		return fmt.Errorf("store: put exec: %w", err)
	}
	return nil
}
