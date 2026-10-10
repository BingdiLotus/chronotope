package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/ledger"
	"time"

	"github.com/jackc/pgx/v5"
)

// LLMCallRow 是 ModelCall 账本行（审计 A1——Provider 接受证据 + usage 对账事实）。
// LLMCallRow（期 7 下沉：core/ledger）。
type LLMCallRow = ledger.LLMCallRow

// PutLLMCallPrepared 派发前落 prepared 行（幂等按 run+step：重放/崩溃窗口
// 的第二次闭包执行更新同一行——dispatch 证据保留供对账）。
func (s *Store) PutLLMCallPrepared(ctx context.Context, runID string, step int, requestHash string) (*LLMCallRow, error) {
	const q = `
INSERT INTO llm_calls (run_id, step, state, prepared_at, request_hash)
VALUES ($1, $2, 'prepared', now(), $3)
ON CONFLICT (run_id, step) DO UPDATE SET dispatch_seq = llm_calls.dispatch_seq + 1,
    request_hash = COALESCE(llm_calls.request_hash, EXCLUDED.request_hash)
RETURNING id, run_id, step, dispatch_seq, state, prepared_at, COALESCE(request_hash, ''), COALESCE(result::text, '')`
	var row LLMCallRow
	var id int64
	var resultRaw string
	if err := s.Pool.QueryRow(ctx, q, runID, step, nullIfEmpty(requestHash)).Scan(&id, &row.RunID, &row.Step, &row.DispatchSeq, &row.State, &row.PreparedAt, &row.RequestHash, &resultRaw); err != nil {
		return nil, fmt.Errorf("store: llm prepared: %w", err)
	}
	row.Result = resultRaw
	return &row, nil
}

// MarkLLMCallDispatched 派发发出标 dispatched（审计 A1：prepared→dispatched
// →result 三态完整——发送证据与结果之间的接受证据）。
func (s *Store) MarkLLMCallDispatched(ctx context.Context, runID string, step int) error {
	const q = `UPDATE llm_calls SET state = 'dispatched', dispatched_at = now() WHERE run_id = $1 AND step = $2 AND state = 'prepared'`
	tag, err := s.Pool.Exec(ctx, q, runID, step)
	if err != nil {
		return fmt.Errorf("store: llm dispatched: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 审计 4.1：已 dispatched/result 的行 0 更新——「上次已发送、结果
		// 未确认」不得被当可再派发
		return fmt.Errorf("store: llm dispatched: %w", ErrDispatchClaimLost)
	}
	return nil
}

// ErrDispatchClaimLost dispatched 转换未赢 claim（审计 4.1——0 行更新）。
var ErrDispatchClaimLost = errors.New("dispatch claim lost (state 非 prepared)")

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
func (s *Store) PutLLMCallResult(ctx context.Context, runID string, step int, tokensIn, tokensOut int64, partial, unknown bool, errMsg, resultJSON string) error {
	state := "result"
	if unknown || errMsg != "" {
		state = "unknown"
	}
	const q = `
UPDATE llm_calls
SET state = $3, tokens_in = $4, tokens_out = $5, usage_partial = $6,
    usage_unknown = $7, err = $8, result_at = now(),
    result = COALESCE($9::jsonb, result)
WHERE run_id = $1 AND step = $2`
	_, err := s.Pool.Exec(ctx, q, runID, step, state, tokensIn, tokensOut, partial, unknown, nullIfEmpty(errMsg), nullIfEmpty(resultJSON))
	if err != nil {
		return fmt.Errorf("store: llm result: %w", err)
	}
	return nil
}

// MCPCallRow 是 MCP 效果账本行（审计 A2——外部效果证据）。
// MCPCallRow（期 7 下沉：core/ledger）。
type MCPCallRow = ledger.MCPCallRow

// PutMCPCallPrepared 派发前落行（调用身份 = call_key——审计 4.3：同 step
// 多工具调用的身份，工具名不是逻辑调用身份；冲突 SELECT 回读既存状态）。
func (s *Store) PutMCPCallPrepared(ctx context.Context, runID string, step int, server, tool, callKey, requestHash string) (*MCPCallRow, error) {
	const q = `
INSERT INTO mcp_calls (run_id, step, server, tool, state, prepared_at, call_key, request_hash)
VALUES ($1, $2, $3, $4, 'prepared', now(), $5, $6)
ON CONFLICT (run_id, step, server, tool, call_key) DO NOTHING
RETURNING run_id, step, server, tool, state, prepared_at, COALESCE(request_hash, ''), COALESCE(result::text, '')`
	var row MCPCallRow
	var resultRaw string
	if err := s.Pool.QueryRow(ctx, q, runID, step, server, tool, nullIfEmpty(callKey), nullIfEmpty(requestHash)).Scan(&row.RunID, &row.Step, &row.Server, &row.Tool, &row.State, &row.PreparedAt, &row.RequestHash, &resultRaw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 冲突（同身份已有行）——SELECT 回读
			const gq = `SELECT run_id, step, server, tool, state, prepared_at, COALESCE(request_hash, ''), COALESCE(result::text, '') FROM mcp_calls WHERE run_id = $1 AND step = $2 AND server = $3 AND tool = $4 AND call_key = $5`
			if gErr := s.Pool.QueryRow(ctx, gq, runID, step, server, tool, callKey).Scan(&row.RunID, &row.Step, &row.Server, &row.Tool, &row.State, &row.PreparedAt, &row.RequestHash, &resultRaw); gErr != nil {
				return nil, fmt.Errorf("store: mcp prepared readback: %w", gErr)
			}
			row.Result = resultRaw
			return &row, nil
		}
		return nil, fmt.Errorf("store: mcp prepared: %w", err)
	}
	row.Result = resultRaw
	return &row, nil
}

// PutMCPCallResult 结果落账（错误 → unknown——网络超时可能效果已发生）。
func (s *Store) PutMCPCallResult(ctx context.Context, runID string, step int, server, tool, callKey, requestHash, resultJSON, errMsg string) error {
	state := "result"
	if errMsg != "" {
		state = "unknown"
	}
	const q = `
UPDATE mcp_calls SET state = $5, err = $6, result_at = now(),
    result = COALESCE($7::jsonb, result), request_hash = COALESCE($8, request_hash)
WHERE run_id = $1 AND step = $2 AND server = $3 AND tool = $4 AND call_key = $9`
	tag, err := s.Pool.Exec(ctx, q, runID, step, server, tool, state, nullIfEmpty(errMsg), nullIfEmpty(resultJSON), nullIfEmpty(requestHash), nullIfEmpty(callKey))
	if err != nil {
		return fmt.Errorf("store: mcp result: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 审计 4.1（九期）：同身份 0 行——结果合同不得静默（旧四列范围的
		// 多行污染关闭）
		return fmt.Errorf("store: mcp result: %w", ErrDispatchClaimLost)
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
