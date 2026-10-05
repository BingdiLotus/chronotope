package store

import (
	"context"
	"fmt"
	"time"
)

// UsageRow 是 usage 表行（三轴计量：活跃秒 / token / 计算秒，1min 桶）。
type UsageRow struct {
	SessionID      string
	Bucket         time.Time
	ActiveSeconds  float64
	TokensIn       int64
	TokensOut      int64
	ComputeSeconds float64
}

// ResetSessionUsage 清空会话全部用量桶（重建式聚合的第一步，幂等）。
func (s *Store) ResetSessionUsage(ctx context.Context, sessionID string) error {
	const q = `DELETE FROM usage WHERE session_id = $1`
	if _, err := s.Pool.Exec(ctx, q, sessionID); err != nil {
		return fmt.Errorf("store: reset usage: %w", err)
	}
	return nil
}

// UpsertUsage 累积 1min 桶（主键 session_id+bucket，冲突累加）。
func (s *Store) UpsertUsage(ctx context.Context, u UsageRow) error {
	const q = `
INSERT INTO usage (session_id, bucket, active_seconds, tokens_in, tokens_out, compute_seconds)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (session_id, bucket) DO UPDATE SET
  active_seconds  = usage.active_seconds  + EXCLUDED.active_seconds,
  tokens_in       = usage.tokens_in       + EXCLUDED.tokens_in,
  tokens_out      = usage.tokens_out      + EXCLUDED.tokens_out,
  compute_seconds = usage.compute_seconds + EXCLUDED.compute_seconds`
	if _, err := s.Pool.Exec(ctx, q, u.SessionID, u.Bucket, u.ActiveSeconds, u.TokensIn, u.TokensOut, u.ComputeSeconds); err != nil {
		return fmt.Errorf("store: upsert usage: %w", err)
	}
	return nil
}

// ListUsage 按桶升序读会话用量。
func (s *Store) ListUsage(ctx context.Context, sessionID string) ([]UsageRow, error) {
	const q = `
SELECT session_id, bucket, active_seconds, tokens_in, tokens_out, compute_seconds
FROM usage WHERE session_id = $1 ORDER BY bucket`
	rows, err := s.Pool.Query(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list usage: %w", err)
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.SessionID, &u.Bucket, &u.ActiveSeconds, &u.TokensIn, &u.TokensOut, &u.ComputeSeconds); err != nil {
			return nil, fmt.Errorf("store: scan usage: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListEventsAfterID 按全局事件 id 增序扫描（用量聚合器的水位游标；id 全局单调）。
func (s *Store) ListEventsAfterID(ctx context.Context, afterID int64, limit int) ([]EventRow, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	const q = `
SELECT id, session_id, COALESCE(run_id, ''), seq, type, payload, at
FROM events WHERE id > $1 ORDER BY id LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list events after id: %w", err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.ID, &e.SessionID, &e.RunID, &e.Seq, &e.Type, &e.Payload, &e.At); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListSessionsByOrg 会话列表（控制台最小页；近 limit 条倒序）。
func (s *Store) ListSessionsByOrg(ctx context.Context, orgID string, limit int) ([]Session, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	const q = `
SELECT id, org_id, agent_id, status, restate_key, last_active_at, deleted_at
FROM sessions WHERE org_id = $1 AND deleted_at IS NULL
ORDER BY COALESCE(last_active_at, created_at) DESC LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.OrgID, &sess.AgentID, &sess.Status, &sess.RestateKey, &sess.LastActiveAt, &sess.DeletedAt); err != nil {
			return nil, fmt.Errorf("store: scan session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}
