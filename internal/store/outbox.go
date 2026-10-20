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

// FinalizeRun commits terminal state and result together. Replaying the same
// terminal result is a no-op; a different status, session, kind or result conflicts.
func (s *Store) FinalizeRun(ctx context.Context, runID, sessionID string, status sessionapi.RunStatus, kind string, payload json.RawMessage) error {
	if status != sessionapi.RunCompleted && status != sessionapi.RunFailed && status != sessionapi.RunCanceled {
		return fmt.Errorf("store: finalize requires a terminal status")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: finalize begin: %w", err)
	}
	defer tx.Rollback(ctx)
	var current sessionapi.RunStatus
	var owner string
	if err := tx.QueryRow(ctx, `SELECT status,session_id FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&current, &owner); err != nil {
		return fmt.Errorf("store: finalize read: %w", err)
	}
	if owner != sessionID {
		return ErrFinalizeConflict
	}
	if current == sessionapi.RunCompleted || current == sessionapi.RunFailed || current == sessionapi.RunCanceled {
		var same bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deliverables WHERE run_id=$1 AND session_id=$2 AND kind=$3 AND payload=$4::jsonb)`, runID, sessionID, kind, payload).Scan(&same); err != nil {
			return err
		}
		if current == status && same {
			return nil
		}
		return ErrFinalizeConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status=$2,finished_at=now() WHERE id=$1`, runID, string(status)); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO deliverables(run_id,session_id,kind,payload) VALUES($1,$2,$3,$4)
ON CONFLICT(run_id) DO UPDATE SET payload=EXCLUDED.payload
WHERE deliverables.session_id=EXCLUDED.session_id AND deliverables.kind=EXCLUDED.kind AND deliverables.payload=EXCLUDED.payload`, runID, sessionID, kind, payload)
	if err != nil {
		return fmt.Errorf("store: finalize result: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrFinalizeConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: finalize commit: %w", err)
	}
	return nil
}

// CompletedOrphan 已完成但无 completed 事件（终态通知补交的扫描源）。
type CompletedOrphan struct {
	RunID     string
	SessionID string
}

// ListCompletedWithoutEvent 审计 4.2（九期）：run 已 completed 但无
// run.completed 事件的孤儿——补交义务的精确源。
func (s *Store) ListCompletedWithoutEvent(ctx context.Context, limit int) ([]CompletedOrphan, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `SELECT r.id, r.session_id FROM runs r
WHERE r.status = 'completed' AND NOT EXISTS (
  SELECT 1 FROM events e WHERE e.session_id = r.session_id AND e.type = 'run.completed' AND e.run_id = r.id
) ORDER BY r.finished_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: completed orphans: %w", err)
	}
	defer rows.Close()
	var out []CompletedOrphan
	for rows.Next() {
		var o CompletedOrphan
		if err := rows.Scan(&o.RunID, &o.SessionID); err != nil {
			return nil, fmt.Errorf("store: scan completed orphan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
