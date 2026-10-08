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

// UsageAggregate 是 org 用量聚合行（期 5 §A：管理面用量端点）。
type UsageAggregate struct {
	Bucket         time.Time `json:"bucket"`
	ActiveSeconds  float64   `json:"active_seconds"`
	TokensIn       int64     `json:"tokens_in"`
	TokensOut      int64     `json:"tokens_out"`
	ComputeSeconds float64   `json:"compute_seconds"`
}

// AggregateOrgUsage 按粒度聚合 org 用量（usage 表经 sessions 关联 org；
// 粒度 hour|day|month——bucket 截断）。
func (s *Store) AggregateOrgUsage(ctx context.Context, orgID, granularity string) ([]UsageAggregate, error) {
	trunc := map[string]string{
		"hour":  "hour",
		"day":   "day",
		"month": "month",
	}[granularity]
	if trunc == "" {
		trunc = "day"
	}
	q := fmt.Sprintf(`
SELECT date_trunc('%s', u.bucket) AS bucket,
       sum(u.active_seconds), sum(u.tokens_in), sum(u.tokens_out), sum(u.compute_seconds)
FROM usage u JOIN sessions se ON se.id = u.session_id
WHERE se.org_id = $1
GROUP BY 1 ORDER BY 1`, trunc)
	rows, err := s.Pool.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: aggregate usage: %w", err)
	}
	defer rows.Close()
	var out []UsageAggregate
	for rows.Next() {
		var a UsageAggregate
		if err := rows.Scan(&a.Bucket, &a.ActiveSeconds, &a.TokensIn, &a.TokensOut, &a.ComputeSeconds); err != nil {
			return nil, fmt.Errorf("store: scan usage aggregate: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
