package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// UsageWatermark 是增量 rollup 水位行（期 2 §B）。
type UsageWatermark struct {
	LastEventID int64     `json:"last_event_id"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GetUsageWatermark 读水位（无行 → 0）。
func (s *Store) GetUsageWatermark(ctx context.Context) (int64, error) {
	const q = `SELECT last_event_id FROM usage_watermark WHERE id = 1`
	var id int64
	err := s.Pool.QueryRow(ctx, q).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: get usage watermark: %w", err)
	}
	return id, nil
}

// ApplyUsageDelta 原子应用增量：加算 usage 行 + 推进水位（同事务——崩溃时
// 水位未推进 → 重放同批 delta，幂等）。fromID 校验防并发双跑错位。
func (s *Store) ApplyUsageDelta(ctx context.Context, fromID, toID int64, deltas []UsageRow) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: delta begin: %w", err)
	}
	defer tx.Rollback(ctx)
	const upsert = `
INSERT INTO usage (session_id, bucket, active_seconds, tokens_in, tokens_out, compute_seconds)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (session_id, bucket) DO UPDATE
SET active_seconds = usage.active_seconds + EXCLUDED.active_seconds,
    tokens_in = usage.tokens_in + EXCLUDED.tokens_in,
    tokens_out = usage.tokens_out + EXCLUDED.tokens_out,
    compute_seconds = usage.compute_seconds + EXCLUDED.compute_seconds`
	for _, u := range deltas {
		if _, err := tx.Exec(ctx, upsert, u.SessionID, u.Bucket,
			u.ActiveSeconds, u.TokensIn, u.TokensOut, u.ComputeSeconds); err != nil {
			return fmt.Errorf("store: apply delta: %w", err)
		}
	}
	const wm = `
UPDATE usage_watermark SET last_event_id = $1, updated_at = now()
WHERE id = 1 AND last_event_id = $2`
	tag, err := tx.Exec(ctx, wm, toID, fromID)
	if err != nil {
		return fmt.Errorf("store: advance watermark: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: watermark 竞态（期望 %d 实际已推进）", fromID)
	}
	return tx.Commit(ctx)
}

// RunStartedAt 读 run 的起始事件时间（delta 聚合的活动秒跨度基准；无 → ErrNotFound）。
func (s *Store) RunStartedAt(ctx context.Context, runID string) (time.Time, error) {
	const q = `SELECT at FROM events WHERE run_id = $1 AND type = 'run.started' ORDER BY seq LIMIT 1`
	var at time.Time
	err := s.Pool.QueryRow(ctx, q, runID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: run started at: %w", err)
	}
	return at, nil
}
