package store

import (
	"context"
	"fmt"
	"time"
)

// ExecutorRow 是 executor 注册行（期 4 §B：多宿主池的心跳/能力注册）。
type ExecutorRow struct {
	ID           string    `json:"id"`
	OrgID        string    `json:"org_id"`
	Kind         string    `json:"kind"` // docker | e2b_selfhosted
	Endpoint     string    `json:"endpoint"`
	HeartbeatAt  time.Time `json:"heartbeat_at"`
	Capabilities []byte    `json:"-"`
}

// UpsertExecutor 注册/心跳（幂等覆盖；heartbeat 推进）。
func (s *Store) UpsertExecutor(ctx context.Context, e ExecutorRow) error {
	const q = `
INSERT INTO executors (id, org_id, kind, endpoint, capabilities, status, heartbeat_at)
VALUES ($1, $2, $3, $4, $5, 'up', now())
ON CONFLICT (id) DO UPDATE SET
  endpoint = EXCLUDED.endpoint, kind = EXCLUDED.kind, capabilities = EXCLUDED.capabilities,
  heartbeat_at = now(), status = 'up'`
	if _, err := s.Pool.Exec(ctx, q, e.ID, e.OrgID, e.Kind, e.Endpoint, e.Capabilities); err != nil {
		return fmt.Errorf("store: upsert executor: %w", err)
	}
	return nil
}

// ListHealthyExecutors 心跳新鲜（freshness 内）的 up 执行器——池的候选集。
func (s *Store) ListHealthyExecutors(ctx context.Context, freshness time.Duration) ([]ExecutorRow, error) {
	const q = `
SELECT id, org_id, kind, endpoint, capabilities, COALESCE(heartbeat_at, now())
FROM executors WHERE status = 'up' AND heartbeat_at > now() - $1::interval
ORDER BY id`
	rows, err := s.Pool.Query(ctx, q, fmt.Sprintf("%d seconds", int(freshness.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("store: list executors: %w", err)
	}
	defer rows.Close()
	var out []ExecutorRow
	for rows.Next() {
		var e ExecutorRow
		if err := rows.Scan(&e.ID, &e.OrgID, &e.Kind, &e.Endpoint, &e.Capabilities, &e.HeartbeatAt); err != nil {
			return nil, fmt.Errorf("store: scan executor: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
