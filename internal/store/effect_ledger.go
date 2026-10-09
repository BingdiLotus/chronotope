package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
ON CONFLICT (run_id, step) DO UPDATE SET dispatch_seq = llm_calls.dispatch_seq + 1
RETURNING id, run_id, step, dispatch_seq, state, prepared_at`
	var row LLMCallRow
	var id int64
	if err := s.Pool.QueryRow(ctx, q, runID, step).Scan(&id, &row.RunID, &row.Step, &row.DispatchSeq, &row.State, &row.PreparedAt); err != nil {
		return nil, fmt.Errorf("store: llm prepared: %w", err)
	}
	return &row, nil
}

// MarkLLMCallDispatched 派发发出标 dispatched（审计 A1：prepared→dispatched
// →result 三态完整——发送证据与结果之间的接受证据）。
func (s *Store) MarkLLMCallDispatched(ctx context.Context, runID string, step int) error {
	const q = `UPDATE llm_calls SET state = 'dispatched', dispatched_at = now() WHERE run_id = $1 AND step = $2 AND state = 'prepared'`
	if _, err := s.Pool.Exec(ctx, q, runID, step); err != nil {
		return fmt.Errorf("store: llm dispatched: %w", err)
	}
	return nil
}

// ListCallsForRun 对账查询（审计 A1/A2 的消费端：llm+mcp 账本行）。
func (s *Store) ListCallsForRun(ctx context.Context, runID string) ([]map[string]any, error) {
	var out []map[string]any
	llmRows, err := s.Pool.Query(ctx, `SELECT step, dispatch_seq, state, tokens_in, tokens_out, usage_partial, usage_unknown, COALESCE(err,''), prepared_at, dispatched_at, result_at FROM llm_calls WHERE run_id=$1 ORDER BY step`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list llm calls: %w", err)
	}
	defer llmRows.Close()
	for llmRows.Next() {
		var step, seq int
		var state string
		var tin, tout int64
		var partial, unknown bool
		var errMsg string
		var pa time.Time
		var da, ra *time.Time
		if err := llmRows.Scan(&step, &seq, &state, &tin, &tout, &partial, &unknown, &errMsg, &pa, &da, &ra); err != nil {
			return nil, fmt.Errorf("store: scan llm call: %w", err)
		}
		out = append(out, map[string]any{
			"kind": "llm", "step": step, "dispatch_seq": seq, "state": state,
			"tokens_in": tin, "tokens_out": tout, "usage_partial": partial,
			"usage_unknown": unknown, "err": errMsg, "prepared_at": pa,
			"dispatched_at": da, "result_at": ra,
		})
	}
	mcpRows, err := s.Pool.Query(ctx, `SELECT step, server, tool, state, COALESCE(err,''), prepared_at, result_at FROM mcp_calls WHERE run_id=$1 ORDER BY step`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp calls: %w", err)
	}
	defer mcpRows.Close()
	for mcpRows.Next() {
		var step int
		var server, tool, state, errMsg string
		var pa time.Time
		var ra *time.Time
		if err := mcpRows.Scan(&step, &server, &tool, &state, &errMsg, &pa, &ra); err != nil {
			return nil, fmt.Errorf("store: scan mcp call: %w", err)
		}
		out = append(out, map[string]any{
			"kind": "mcp", "step": step, "server": server, "tool": tool,
			"state": state, "err": errMsg, "prepared_at": pa, "result_at": ra,
		})
	}
	return out, nil
}

// AdmissionState 查询接纳派发状态（审计 B1 的消费端）。
func (s *Store) AdmissionState(ctx context.Context, runID string) (string, error) {
	const q = `SELECT COALESCE(state, '') FROM admission_outbox WHERE run_id = $1`
	var state string
	if err := s.Pool.QueryRow(ctx, q, runID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil // 历史 run 无 outbox 行
		}
		return "", fmt.Errorf("store: admission state: %w", err)
	}
	return state, nil
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

// PutMCPCallPrepared 派发前落行（冲突读既存状态——仲裁合同：result/unknown
// 不得重派发）。
func (s *Store) PutMCPCallPrepared(ctx context.Context, runID string, step int, server, tool string) (*MCPCallRow, error) {
	const q = `
INSERT INTO mcp_calls (run_id, step, server, tool, state, prepared_at)
VALUES ($1, $2, $3, $4, 'prepared', now())
ON CONFLICT (run_id, step, server, tool) DO NOTHING
RETURNING run_id, step, server, tool, state, prepared_at`
	var row MCPCallRow
	if err := s.Pool.QueryRow(ctx, q, runID, step, server, tool).Scan(&row.RunID, &row.Step, &row.Server, &row.Tool, &row.State, &row.PreparedAt); err != nil {
		return nil, fmt.Errorf("store: mcp prepared: %w", err)
	}
	return &row, nil
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
