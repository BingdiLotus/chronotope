package store

import (
	"context"
	"fmt"
	"time"
)

// LLMCallRow 是 ModelCall 账本行（审计 A1——Provider 接受证据 + usage 对账事实）。
type LLMCallRow struct {
	RunID        string
	Step         int
	DispatchSeq  int
	State        string
	TokensIn     int64
	TokensOut    int64
	UsagePartial bool
	UsageUnknown bool
	Err          string
	PreparedAt   time.Time
	DispatchedAt *time.Time
	ResultAt     *time.Time
}

// PutLLMCallPrepared 派发前落 prepared 行（幂等按 run+step：重放/崩溃窗口
// 的第二次闭包执行更新同一行——dispatch 证据保留供对账）。
func (s *Store) PutLLMCallPrepared(ctx context.Context, runID string, step int) (*LLMCallRow, error) {
	const q = `
INSERT INTO llm_calls (run_id, step, state, prepared_at)
VALUES ($1, $2, 'prepared', now())
ON CONFLICT (run_id, step) DO NOTHING
RETURNING id, run_id, step, dispatch_seq, state, prepared_at`
	var row LLMCallRow
	var id int64
	if err := s.Pool.QueryRow(ctx, q, runID, step).Scan(&id, &row.RunID, &row.Step, &row.DispatchSeq, &row.State, &row.PreparedAt); err != nil {
		return nil, fmt.Errorf("store: llm prepared: %w", err)
	}
	return &row, nil
}

// PutLLMCallResult 结果落账（usage 三字段 + 状态 result/unknown）。
func (s *Store) PutLLMCallResult(ctx context.Context, runID string, step int, tokensIn, tokensOut int64, partial, unknown bool, errMsg string) error {
	state := "result"
	if unknown || errMsg != "" {
		state = "unknown"
	}
	const q = `
UPDATE llm_calls
SET state = $3, tokens_in = $4, tokens_out = $5, usage_partial = $6,
    usage_unknown = $7, err = $8, result_at = now()
WHERE run_id = $1 AND step = $2`
	_, err := s.Pool.Exec(ctx, q, runID, step, state, tokensIn, tokensOut, partial, unknown, nullIfEmpty(errMsg))
	if err != nil {
		return fmt.Errorf("store: llm result: %w", err)
	}
	return nil
}

// MCPCallRow 是 MCP 效果账本行（审计 A2——外部效果证据）。
type MCPCallRow struct {
	RunID      string
	Step       int
	Server     string
	Tool       string
	State      string
	Err        string
	PreparedAt time.Time
}

// PutMCPCallPrepared 派发前落行（幂等按 run+step+server+tool）。
func (s *Store) PutMCPCallPrepared(ctx context.Context, runID string, step int, server, tool string) error {
	const q = `
INSERT INTO mcp_calls (run_id, step, server, tool, state, prepared_at)
VALUES ($1, $2, $3, $4, 'prepared', now())
ON CONFLICT (run_id, step, server, tool) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, runID, step, server, tool); err != nil {
		return fmt.Errorf("store: mcp prepared: %w", err)
	}
	return nil
}

// PutMCPCallResult 结果落账（错误 → unknown——网络超时可能效果已发生）。
func (s *Store) PutMCPCallResult(ctx context.Context, runID string, step int, server, tool, errMsg string) error {
	state := "result"
	if errMsg != "" {
		state = "unknown"
	}
	const q = `
UPDATE mcp_calls SET state = $5, err = $6, result_at = now()
WHERE run_id = $1 AND step = $2 AND server = $3 AND tool = $4`
	if _, err := s.Pool.Exec(ctx, q, runID, step, server, tool, state, nullIfEmpty(errMsg)); err != nil {
		return fmt.Errorf("store: mcp result: %w", err)
	}
	return nil
}

// AdmissionPending 是接纳派发的 pending 行（审计 B1——重投扫描的精确源）。
type AdmissionPending struct {
	RunID     string
	SessionID string
	Input     string
	Topic     string
}

// MarkAdmissionDispatched 派发成功标 dispatched（handlers 的 ingress 成功后）。
func (s *Store) MarkAdmissionDispatched(ctx context.Context, runID string) error {
	const q = `UPDATE admission_outbox SET state = 'dispatched', dispatched_at = now() WHERE run_id = $1`
	if _, err := s.Pool.Exec(ctx, q, runID); err != nil {
		return fmt.Errorf("store: admission dispatched: %w", err)
	}
	return nil
}

// ListPendingAdmissions 扫 pending（派发前崩溃的孤儿——精确重投源；超时
// 窗口防「在途派发」误重投）。
func (s *Store) ListPendingAdmissions(ctx context.Context, olderThan time.Duration, limit int) ([]AdmissionPending, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	const q = `
SELECT a.run_id, r.session_id, COALESCE(r.input, ''), COALESCE(r.topic, '')
FROM admission_outbox a
JOIN runs r ON r.id = a.run_id
WHERE a.state = 'pending' AND a.created_at < now() - $1::interval
ORDER BY a.created_at
LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, fmt.Sprintf("%d seconds", int(olderThan.Seconds())), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list pending admissions: %w", err)
	}
	defer rows.Close()
	var out []AdmissionPending
	for rows.Next() {
		var a AdmissionPending
		if err := rows.Scan(&a.RunID, &a.SessionID, &a.Input, &a.Topic); err != nil {
			return nil, fmt.Errorf("store: scan pending admission: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
