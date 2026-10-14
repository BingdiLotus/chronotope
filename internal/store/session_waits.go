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
// 修正方案 P1-2：纯 INSERT 语义——冲突即 no-op（保持已解决状态；
// 重放不复活已 resolved 的 wait）。
func (s *Store) RegisterWait(ctx context.Context, sessionID string, kind WaitKind, handle, intent, expect string) error {
	const q = `
INSERT INTO session_waits (session_id, kind, handle, intent, expect, registered_at, resolved_at)
VALUES ($1, $2, $3, $4, $5, now(), NULL)
ON CONFLICT (session_id, kind, handle) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, sessionID, string(kind), handle, intent, expect); err != nil {
		return fmt.Errorf("store: register wait: %w", err)
	}
	return nil
}

// RenewWait 显式复活（同 handle 新一轮等待的调用方——P1-2 拆分：
// 需要覆盖已解决状态时用此方法，不用 upsert 隐式复活）。
func (s *Store) RenewWait(ctx context.Context, sessionID string, kind WaitKind, handle, intent, expect string) error {
	const q = `
INSERT INTO session_waits (session_id, kind, handle, intent, expect, registered_at, resolved_at)
VALUES ($1, $2, $3, $4, $5, now(), NULL)
ON CONFLICT (session_id, kind, handle) DO UPDATE
SET intent = EXCLUDED.intent, expect = EXCLUDED.expect,
    registered_at = now(), resolved_at = NULL`
	if _, err := s.Pool.Exec(ctx, q, sessionID, string(kind), handle, intent, expect); err != nil {
		return fmt.Errorf("store: renew wait: %w", err)
	}
	return nil
}

// ResolveWait 解决等待（条件满足/超时/取消）。修正方案 P1-1：返回
// RowsAffected——「解决成功」与「静默失败」在类型上可区分。
func (s *Store) ResolveWait(ctx context.Context, sessionID string, kind WaitKind, handle string) (bool, error) {
	const q = `UPDATE session_waits SET resolved_at = now() WHERE session_id = $1 AND kind = $2 AND handle = $3 AND resolved_at IS NULL`
	tag, err := s.Pool.Exec(ctx, q, sessionID, string(kind), handle)
	if err != nil {
		return false, fmt.Errorf("store: resolve wait: %w", err)
	}
	return tag.RowsAffected() > 0, nil
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
