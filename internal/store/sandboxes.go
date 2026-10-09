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
// 绝不复跑命令——契约规范 §4；TTL 30 天（008 跨周恢复意图；结果 >256KB 外置）。
type ExecRow struct {
	IdempotencyKey string
	SandboxID      string
	Result         json.RawMessage
	ExpiresAt      time.Time
	State          string     // prepared|done
	InputDigest    string     // 同键同输入校验（sha256(input)）
	PreparedAt     *time.Time // prepared 时间（in-flight 新鲜度判断）
}

// GetExec 读取幂等缓存；未命中返回 ErrNotFound。
// GetExec 取幂等缓存行（含状态机与输入摘要——评审 #1 闭合依据）。
func (s *Store) GetExec(ctx context.Context, idempotencyKey string) (*ExecRow, error) {
	const q = `SELECT idempotency_key, sandbox_id, result, expires_at, state, input_digest, prepared_at
FROM sandbox_execs WHERE idempotency_key = $1`
	var e ExecRow
	err := s.Pool.QueryRow(ctx, q, idempotencyKey).Scan(
		&e.IdempotencyKey, &e.SandboxID, &e.Result, &e.ExpiresAt, &e.State, &e.InputDigest, &e.PreparedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get exec: %w", err)
	}
	if time.Now().After(e.ExpiresAt) {
		return nil, ErrNotFound // 缓存过期视为未执行（TTL 30 天）
	}
	return &e, nil
}

// PutExecPrepared 执行前 claim（prepared）。返回 won：本轮插入成功 = 获得单执行权
// ——并发同键两请求都先 GetExec cache miss 时，只有一个 INSERT 生效（审计 #1：
// 旧实现 ON CONFLICT DO NOTHING 无条件返回 nil，败者无感知 → 双执行）。
func (s *Store) PutExecPrepared(ctx context.Context, idempotencyKey, sandboxID, inputDigest string) (bool, *time.Time, error) {
	const q = `
INSERT INTO sandbox_execs (idempotency_key, sandbox_id, result, expires_at, state, input_digest, prepared_at)
VALUES ($1, $2, NULL, now() + interval '30 days', 'prepared', $3, now())
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING idempotency_key, prepared_at`
	var returned string
	var pa time.Time
	if err := s.Pool.QueryRow(ctx, q, idempotencyKey, sandboxID, inputDigest).Scan(&returned, &pa); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil, nil // 未插入 = 败者（已有 prepared 行）
		}
		return false, nil, fmt.Errorf("store: prepared exec: %w", err)
	}
	return true, &pa, nil
}

// UpdateExecState 标记 exec 行状态（审计 A3：prepared 过期停派发 → unknown——
// 查询/对账可见；管理员接管删除后重新 claim 可执行）。
func (s *Store) UpdateExecState(ctx context.Context, idempotencyKey, state string) error {
	const q = `UPDATE sandbox_execs SET state = $2 WHERE idempotency_key = $1`
	if _, err := s.Pool.Exec(ctx, q, idempotencyKey, state); err != nil {
		return fmt.Errorf("store: exec state: %w", err)
	}
	return nil
}

// DeleteExec 管理员接管：删除 exec 行（prepared/unknown 卡死 → 重新 claim）。
func (s *Store) DeleteExec(ctx context.Context, idempotencyKey string) error {
	const q = `DELETE FROM sandbox_execs WHERE idempotency_key = $1`
	if _, err := s.Pool.Exec(ctx, q, idempotencyKey); err != nil {
		return fmt.Errorf("store: delete exec: %w", err)
	}
	return nil
}

// PutExecDone 执行完成落账（状态 done；失败返回错误——调用方必须发 error 帧，
// 不得回成功 exit，否则未知窗口内重试会双执行，评审 #1）。
// 审计准入 #2：prepared_at 条件——旧 holder 的迟到结果（prepared 已换代）
// 不得覆盖 canonical done。
func (s *Store) PutExecDone(ctx context.Context, idempotencyKey, sandboxID string, result json.RawMessage, preparedAt *time.Time) error {
	const q = `
UPDATE sandbox_execs
SET result = $3, state = 'done', expires_at = now() + interval '30 days', sandbox_id = $2
WHERE idempotency_key = $1 AND prepared_at = $4`
	tag, err := s.Pool.Exec(ctx, q, idempotencyKey, sandboxID, result, preparedAt)
	if err != nil {
		return fmt.Errorf("store: done exec: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 审计 6.1：UPDATE 0 行（prepared 已换代/不存在）不得对外成功
		return fmt.Errorf("store: done exec: %w", ErrExecClaimLost)
	}
	return nil
}

// PutExec 写入幂等缓存（TTL 30 天，契约规范 §4）。
// GetSandboxBySession 取会话最新沙箱行（快照恢复依据：ensureSandbox 重建时读
// snapshot_ref——评审 #7 恢复路径）。
func (s *Store) GetSandboxBySession(ctx context.Context, sessionID string) (*SandboxRow, error) {
	const q = `
SELECT sandbox_id, org_id, session_id, driver, container_ref, image, limits, tier,
       snapshot_ref, file_sync_state, ttl, status, created_at
FROM sandboxes WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`
	var (
		sb         SandboxRow
		limitsJSON json.RawMessage
		ttl        pgtype.Interval
	)
	err := s.Pool.QueryRow(ctx, q, sessionID).Scan(
		&sb.SandboxID, &sb.OrgID, &sb.SessionID, &sb.Driver, &sb.ContainerRef, &sb.Image,
		&limitsJSON, &sb.Tier, &sb.SnapshotRef, &sb.FileSyncState, &ttl, &sb.Status, &sb.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get sandbox by session: %w", err)
	}
	_ = json.Unmarshal(limitsJSON, &sb.Limits)
	if ttl.Valid {
		d := time.Duration(ttl.Microseconds+int64(ttl.Days)*24*3600*1e6) * time.Microsecond
		sb.TTL = &d
	}
	return &sb, nil
}

// ListExpiredSandboxes 孤儿 GC 扫描（W8）：ttl 过期且未删除的沙箱。
// executorID 非空时只扫归属该 executor 的行（审计 P0-3：多宿主按 owner 过滤）。
func (s *Store) ListExpiredSandboxes(ctx context.Context, now time.Time, executorID string) ([]*SandboxRow, error) {
	q := `
SELECT s.sandbox_id, s.org_id, s.session_id, s.driver, s.container_ref, s.image, s.limits, s.tier,
       s.snapshot_ref, s.file_sync_state, s.ttl, s.status, s.created_at
FROM sandboxes s
LEFT JOIN sandbox_leases l ON l.sandbox_id = s.sandbox_id AND l.expires_at > now()
WHERE s.ttl IS NOT NULL AND s.created_at + s.ttl < $1
  AND s.status <> 'deleted'
  AND l.sandbox_id IS NULL`
	args := []any{now}
	if executorID != "" {
		q += ` AND COALESCE(s.executor_id, '') = $2`
		args = append(args, executorID)
	}
	q += `
ORDER BY s.created_at`
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list expired sandboxes: %w", err)
	}
	defer rows.Close()
	var out []*SandboxRow
	for rows.Next() {
		var (
			sb         SandboxRow
			limitsJSON json.RawMessage
			ttl        pgtype.Interval
		)
		if err := rows.Scan(&sb.SandboxID, &sb.OrgID, &sb.SessionID, &sb.Driver, &sb.ContainerRef, &sb.Image,
			&limitsJSON, &sb.Tier, &sb.SnapshotRef, &sb.FileSyncState, &ttl, &sb.Status, &sb.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan expired sandbox: %w", err)
		}
		_ = json.Unmarshal(limitsJSON, &sb.Limits)
		if ttl.Valid {
			d := time.Duration(ttl.Microseconds+int64(ttl.Days)*24*3600*1e6) * time.Microsecond
			sb.TTL = &d
		}
		out = append(out, &sb)
	}
	return out, rows.Err()
}

// DeleteSandbox 删除沙箱事实行（GC 清理成功后调用）。
func (s *Store) DeleteSandbox(ctx context.Context, sandboxID string) error {
	// 审计准入 #4：lease 行先删（FK NO ACTION + Release 置过期保留行——
	// 沙箱删除时其 lease tombstone 一并清理；generation 单调的 fencing
	// 证据只对活跃沙箱有意义）。同事务——半删状态不落库。
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: delete sandbox begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM sandbox_leases WHERE sandbox_id = $1`, sandboxID); err != nil {
		return fmt.Errorf("store: delete sandbox lease: %w", err)
	}
	const q = `DELETE FROM sandboxes WHERE sandbox_id = $1`
	tag, err := tx.Exec(ctx, q, sandboxID)
	if err != nil {
		return fmt.Errorf("store: delete sandbox: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: delete sandbox commit: %w", err)
	}
	return nil
}

// ErrExecClaimLost done 落账的 claim 已失效（审计 6.1——0 行 UPDATE）。
var ErrExecClaimLost = errors.New("exec claim lost (prepared_at 不匹配)")

// ErrLeaseOwnerMismatch 租约持有者不匹配（审计 P0-1 的 owner CAS 拒绝）。
var ErrLeaseOwnerMismatch = errors.New("lease owner mismatch")

// LeaseRow 是沙箱租约行。
type LeaseRow struct {
	SandboxID  string    `json:"sandbox_id"`
	RunID      string    `json:"run_id"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// AcquireLease 获取/续约（UPSERT：存在则 generation+1 并重置过期）。
func (s *Store) AcquireLease(ctx context.Context, sandboxID, runID string, ttl time.Duration) (*LeaseRow, error) {
	const q = `
INSERT INTO sandbox_leases (sandbox_id, run_id, generation, expires_at)
VALUES ($1, $2, 1, now() + $3)
ON CONFLICT (sandbox_id) DO UPDATE
SET run_id = EXCLUDED.run_id, generation = sandbox_leases.generation + 1,
    expires_at = now() + $3
WHERE sandbox_leases.run_id = EXCLUDED.run_id
   OR sandbox_leases.expires_at < now()
RETURNING sandbox_id, run_id, generation, expires_at`
	var row LeaseRow
	if err := s.Pool.QueryRow(ctx, q, sandboxID, runID, ttl).Scan(
		&row.SandboxID, &row.RunID, &row.Generation, &row.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// owner CAS 失败（审计 P0-1：旧 owner 的续约不得夺走新 holder 的租约）
			return nil, fmt.Errorf("store: acquire lease: %w", ErrLeaseOwnerMismatch)
		}
		return nil, fmt.Errorf("store: acquire lease: %w", err)
	}
	return &row, nil
}

// ReleaseLease 释放（generation 校验：旧持有者携带过期代次不得误释放新租约）。
func (s *Store) ReleaseLease(ctx context.Context, sandboxID string, generation int64) (bool, error) {
	// 置过期而非删行——行永存使 generation 单调（审计 P0-2：删行后下一
	// Acquire 从 1 重启，旧 gen=1 的 release 会删除新 gen=1 的租约——ABA）
	const q = `UPDATE sandbox_leases SET expires_at = now() WHERE sandbox_id = $1 AND generation = $2`
	tag, err := s.Pool.Exec(ctx, q, sandboxID, generation)
	if err != nil {
		return false, fmt.Errorf("store: release lease: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// HasActiveLease 租约是否有效（GC 依赖安全回收的依据）。
func (s *Store) HasActiveLease(ctx context.Context, sandboxID string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM sandbox_leases WHERE sandbox_id = $1 AND expires_at > now())`
	var ok bool
	if err := s.Pool.QueryRow(ctx, q, sandboxID).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: has active lease: %w", err)
	}
	return ok, nil
}

// GetSandboxOwner 取沙箱归属 executor_id（期 4 §B 池路由；无 → 空串）。
func (s *Store) GetSandboxOwner(ctx context.Context, sandboxID string) (string, error) {
	const q = `SELECT COALESCE(executor_id, '') FROM sandboxes WHERE sandbox_id = $1`
	var owner string
	if err := s.Pool.QueryRow(ctx, q, sandboxID).Scan(&owner); err != nil {
		return "", fmt.Errorf("store: sandbox owner: %w", err)
	}
	return owner, nil
}

// SetSandboxOwner 落沙箱归属（池选择后写回）。
func (s *Store) SetSandboxOwner(ctx context.Context, sandboxID, executorID string) error {
	const q = `UPDATE sandboxes SET executor_id = $2 WHERE sandbox_id = $1`
	if _, err := s.Pool.Exec(ctx, q, sandboxID, executorID); err != nil {
		return fmt.Errorf("store: set sandbox owner: %w", err)
	}
	return nil
}

// GetLease 读沙箱租约行（审计 P0-1：Execute 的 holder 校验；无行 → nil）。
func (s *Store) GetLease(ctx context.Context, sandboxID string) (*LeaseRow, error) {
	const q = `SELECT sandbox_id, run_id, generation, expires_at FROM sandbox_leases WHERE sandbox_id = $1`
	var row LeaseRow
	if err := s.Pool.QueryRow(ctx, q, sandboxID).Scan(
		&row.SandboxID, &row.RunID, &row.Generation, &row.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get lease: %w", err)
	}
	return &row, nil
}
