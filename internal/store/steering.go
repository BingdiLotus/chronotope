package store

import (
	"context"
	"fmt"

	"github.com/bingdilotus/chronotope/internal/core/session"
)

// Steering 是运行中转向（期 7 下沉：core/session）。
type Steering = session.Steering

// EnqueueSteering 排队转向（运行中——显式 steer 端点；不打断当前 run）。
func (s *Store) EnqueueSteering(ctx context.Context, sessionID, input string) error {
	const q = `INSERT INTO session_steers (session_id, input) VALUES ($1, $2)`
	if _, err := s.Pool.Exec(ctx, q, sessionID, input); err != nil {
		return fmt.Errorf("store: enqueue steering: %w", err)
	}
	return nil
}

// ListPendingSteerings 未消费的转向（按入队序——run 的 drain 点）。
func (s *Store) ListPendingSteerings(ctx context.Context, sessionID string, limit int) ([]Steering, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, session_id, input, created_at FROM session_steers WHERE session_id = $1 AND consumed_at IS NULL ORDER BY id LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending steerings: %w", err)
	}
	defer rows.Close()
	var out []Steering
	for rows.Next() {
		var st Steering
		if err := rows.Scan(&st.ID, &st.SessionID, &st.Input, &st.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan steering: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ConsumeSteerings 标记消费（注入后——幂等：只标未消费的）。
func (s *Store) ConsumeSteerings(ctx context.Context, sessionID string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE session_steers SET consumed_at = now() WHERE session_id = $1 AND id = ANY($2) AND consumed_at IS NULL`
	if _, err := s.Pool.Exec(ctx, q, sessionID, ids); err != nil {
		return fmt.Errorf("store: consume steerings: %w", err)
	}
	return nil
}
