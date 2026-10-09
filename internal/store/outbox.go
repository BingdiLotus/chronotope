package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
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
	tag, err := s.Pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: mark deliverables delivered: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 审计（M3）：不存在的 id/重复 ack 不得静默成功——投递回执的真实语义
		return fmt.Errorf("store: delivery ack: %w", ErrDeliveryAckConflict)
	}
	return nil
}

// ErrDeliveryAckConflict 投递回执冲突（M3——id 不存在或已 ack）。
var ErrDeliveryAckConflict = errors.New("delivery ack conflict")

// ErrFinalizeConflict 终态提交冲突（审计 5.2——已终态或同 key 异 payload）。
var ErrFinalizeConflict = errors.New("finalize conflict (run 已终态)")

// FinalizeRun E2：终态原子性——run 终态状态 + deliverable 结果引用同事务
// （canonical terminal 与 owner outbox 的原子提交——deliverable 失败不再被吞，
// 终态与交付要么同存要么同无）。
func (s *Store) FinalizeRun(ctx context.Context, runID, sessionID string, status sessionapi.RunStatus, kind string, payload json.RawMessage) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: finalize begin: %w", err)
	}
	defer tx.Rollback(ctx)
	const q = `
UPDATE runs SET status = $2,
  started_at = COALESCE(started_at, CASE WHEN $2 = 'running' THEN now() ELSE started_at END),
  finished_at = CASE WHEN $2 IN ('completed','failed','cancelled') THEN now() ELSE finished_at END
WHERE id = $1`
	if _, err := tx.Exec(ctx, q, runID, string(status)); err != nil {
		return fmt.Errorf("store: finalize status: %w", err)
	}
	const dq = `
INSERT INTO deliverables (run_id, session_id, kind, payload)
VALUES ($1, $2, $3, $4)
ON CONFLICT (run_id) DO UPDATE SET payload = EXCLUDED.payload
WHERE deliverables.payload = EXCLUDED.payload`
	if _, err := tx.Exec(ctx, dq, runID, sessionID, kind, payload); err != nil {
		return fmt.Errorf("store: finalize deliverable: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: finalize commit: %w", err)
	}
	return nil
}
