package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// DeliverableRow 是交付清单行（run 完成 → 投递方消费 → 回执）。
type DeliverableRow struct {
	ID          int64           `json:"id"`
	RunID       string          `json:"run_id"`
	SessionID   string          `json:"session_id"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// CreateDeliverable 写交付行（run_id 唯一——journal 重放幂等吞重复）。
func (s *Store) CreateDeliverable(ctx context.Context, runID, sessionID, kind string, payload json.RawMessage) error {
	const q = `
INSERT INTO deliverables (run_id, session_id, kind, payload)
VALUES ($1, $2, $3, $4)
ON CONFLICT (run_id) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, runID, sessionID, kind, payload); err != nil {
		return fmt.Errorf("store: create deliverables: %w", err)
	}
	return nil
}

// ListDeliverables 会话交付清单（升序；含已投递，投递状态可见）。
func (s *Store) ListDeliverables(ctx context.Context, sessionID string, limit int) ([]*DeliverableRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `
SELECT id, run_id, session_id, kind, payload, delivered_at, created_at
FROM deliverables WHERE session_id = $1 ORDER BY id LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list deliverables: %w", err)
	}
	defer rows.Close()
	var out []*DeliverableRow
	for rows.Next() {
		var row DeliverableRow
		if err := rows.Scan(&row.ID, &row.RunID, &row.SessionID, &row.Kind, &row.Payload, &row.DeliveredAt, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan deliverables: %w", err)
		}
		out = append(out, &row)
	}
	return out, rows.Err()
}

// MarkDeliverableDelivered 投递回执。
func (s *Store) MarkDeliverableDelivered(ctx context.Context, id int64) error {
	const q = `UPDATE deliverables SET delivered_at = now() WHERE id = $1 AND delivered_at IS NULL`
	if _, err := s.Pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("store: mark deliverables delivered: %w", err)
	}
	return nil
}
