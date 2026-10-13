package store

import (
	"context"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/wait"
	"time"
)

// WaitKind 等待类型（期 7 下沉：core/wait）。
type WaitKind = wait.Kind

// WaitKind 常量（core/wait 的别名）。
const (
	WaitTimer         WaitKind = wait.Timer
	WaitTaskResult    WaitKind = wait.TaskResult
	WaitExternal      WaitKind = wait.External
	WaitOperatorInput WaitKind = wait.OperatorInput
)

// RegisterWait 注册等待（yield 时捕获 intent——为什么停 + 期待条件）。
func (s *Store) RegisterWait(ctx context.Context, sessionID string, kind WaitKind, handle, intent, expect string) error {
	const q = `
INSERT INTO session_waits (session_id, kind, handle, intent, expect, registered_at, resolved_at)
VALUES ($1, $2, $3, $4, $5, now(), NULL)
ON CONFLICT (session_id, kind, handle) DO UPDATE
SET intent = EXCLUDED.intent, expect = EXCLUDED.expect,
    registered_at = now(), resolved_at = NULL`
	if _, err := s.Pool.Exec(ctx, q, sessionID, string(kind), handle, intent, expect); err != nil {
		return fmt.Errorf("store: register wait: %w", err)
	}
	return nil
}

// ResolveWait 解决等待（条件满足/超时/取消）。
func (s *Store) ResolveWait(ctx context.Context, sessionID string, kind WaitKind, handle string) error {
	const q = `UPDATE session_waits SET resolved_at = now() WHERE session_id = $1 AND kind = $2 AND handle = $3 AND resolved_at IS NULL`
	if _, err := s.Pool.Exec(ctx, q, sessionID, string(kind), handle); err != nil {
		return fmt.Errorf("store: resolve wait: %w", err)
	}
	return nil
}

// ActiveWaits 活跃等待（DECIDE 的可判定查询——「现在在等什么」）。
func (s *Store) ActiveWaits(ctx context.Context, sessionID string) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, `SELECT kind, handle, intent, expect, registered_at FROM session_waits WHERE session_id = $1 AND resolved_at IS NULL ORDER BY registered_at`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: active waits: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var kind, handle, intent, expect string
		var reg time.Time
		if err := rows.Scan(&kind, &handle, &intent, &expect, &reg); err != nil {
			return nil, fmt.Errorf("store: scan active wait: %w", err)
		}
		out = append(out, map[string]any{"kind": kind, "handle": handle, "intent": intent, "expect": expect, "registered_at": reg})
	}
	return out, rows.Err()
}

// ListExpiredOperatorWaits 审批超时的过期等待（restate-ai 调研①——
// operator_input 未解决且超 TTL——SLA 的降级路径）。
func (s *Store) ListExpiredOperatorWaits(ctx context.Context, olderThan time.Duration) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, `SELECT session_id, handle, intent FROM session_waits WHERE kind = 'operator_input' AND resolved_at IS NULL AND registered_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return nil, fmt.Errorf("store: expired operator waits: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var sid, handle, intent string
		if err := rows.Scan(&sid, &handle, &intent); err != nil {
			return nil, fmt.Errorf("store: scan expired wait: %w", err)
		}
		out = append(out, map[string]any{"session_id": sid, "handle": handle, "intent": intent})
	}
	return out, rows.Err()
}
