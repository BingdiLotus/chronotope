package store

import (
	"context"
	"fmt"
	"time"
)

// Subscribe 注册事件投递订阅（同 session+channel+target 幂等）。
func (s *Store) Subscribe(ctx context.Context, sessionID, channel, target string) error {
	const q = `
INSERT INTO subscriptions (session_id, channel, target)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, sessionID, channel, target); err != nil {
		return fmt.Errorf("store: subscribe: %w", err)
	}
	return nil
}

// Subscription 是投递订阅。
type Subscription struct {
	Channel string `json:"channel"`
	Target  string `json:"target"`
}

// PendingOutboxRow 是待投递行。
type PendingOutboxRow struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	EventID   int64  `json:"event_id"`
	URL       string `json:"url"`
	Channel   string `json:"channel"`
	Attempts  int    `json:"attempts"`
}

// ListPendingOutbox 扫描到期待投递行（投递 worker 拉取）。
func (s *Store) ListPendingOutbox(ctx context.Context, limit int) ([]*PendingOutboxRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
SELECT o.id, o.session_id, o.event_id, o.url, COALESCE(s.channel, 'webhook'), o.attempts
FROM outbox o
LEFT JOIN subscriptions s ON s.session_id = o.session_id AND s.target = o.url
WHERE o.next_at <= now() AND o.attempts < 10
ORDER BY o.id
LIMIT $1`
	rows, err := s.Pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list pending outbox: %w", err)
	}
	defer rows.Close()
	var out []*PendingOutboxRow
	for rows.Next() {
		var row PendingOutboxRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.EventID, &row.URL, &row.Channel, &row.Attempts); err != nil {
			return nil, fmt.Errorf("store: scan pending outbox: %w", err)
		}
		out = append(out, &row)
	}
	return out, rows.Err()
}

// OutboxDelivered 投递成功 → 删除行。
func (s *Store) OutboxDelivered(ctx context.Context, id int64) error {
	const q = `DELETE FROM outbox WHERE id = $1`
	if _, err := s.Pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("store: outbox delivered: %w", err)
	}
	return nil
}

// OutboxRetry 投递失败 → attempts+1 + 指数退避（2^attempts 秒，封顶 5min）。
func (s *Store) OutboxRetry(ctx context.Context, id, attempts int64) error {
	backoff := time.Duration(1<<min(attempts, 8)) * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	const q = `UPDATE outbox SET attempts = attempts + 1, next_at = now() + $2 WHERE id = $1`
	if _, err := s.Pool.Exec(ctx, q, id, backoff); err != nil {
		return fmt.Errorf("store: outbox retry: %w", err)
	}
	return nil
}

// GetEvent 投递载荷（事件类型 + payload + 时间）。
func (s *Store) GetEvent(ctx context.Context, eventID int64) (string, []byte, time.Time, error) {
	const q = `SELECT type, payload, at FROM events WHERE id = $1`
	var typ string
	var payload []byte
	var at time.Time
	if err := s.Pool.QueryRow(ctx, q, eventID).Scan(&typ, &payload, &at); err != nil {
		return "", nil, time.Time{}, fmt.Errorf("store: get event %d: %w", eventID, err)
	}
	return typ, payload, at, nil
}
