package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/bingdilotus/chronotope/internal/core/commitment"
	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/jackc/pgx/v5"
)

// CommitRow 承诺投影行。
type CommitRow struct {
	ID            string
	AgentID       string
	MadeInThread  string
	MadeInRun     string
	Content       string
	ContentDigest string
	State         commitment.State
	DueAt         *string
	Counterparty  string
}

// CommitIntent 创建承诺的意图（出站门判定为正后）。
type CommitIntent struct {
	ID           string
	AgentID      string
	MadeInThread string
	MadeInRun    string
	Counterparty string
	SpeechAct    string
	Content      string
	DueAt        *string
	Confidence   float64
}

// ContentDigest 字节级判重（sha256(content)——修正版 §2.1）。
func ContentDigest(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// PutCommitIntent 幂等写入承诺（事件 agent.committed + 投影行 state=pending）。
// 同 ID 冲突时读回既有行（重放安全——事件去重）。
func (s *Store) PutCommitIntent(ctx context.Context, in CommitIntent) (*CommitRow, error) {
	payload, _ := json.Marshal(map[string]any{
		"id": in.ID, "agent_id": in.AgentID, "thread": in.MadeInThread,
		"content": in.Content, "speech_act": in.SpeechAct,
	})
	// 投影 upsert（幂等）
	const q = `
INSERT INTO agent_commitments (id, agent_id, made_in_thread, made_in_run, counterparty,
  speech_act, content, content_digest, decider_confidence, due_at, state, made_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending', now())
ON CONFLICT (id) DO NOTHING
RETURNING id, agent_id, made_in_thread, COALESCE(made_in_run, ''), content, content_digest, state`
	var row CommitRow
	var conf *float64
	if in.Confidence > 0 {
		conf = &in.Confidence
	}
	err := s.Pool.QueryRow(ctx, q, in.ID, in.AgentID, in.MadeInThread, nullIfEmpty(in.MadeInRun),
		nullIfEmpty(in.Counterparty), in.SpeechAct, in.Content, ContentDigest(in.Content), conf,
		nullIfEmpty(deref(in.DueAt))).Scan(&row.ID, &row.AgentID, &row.MadeInThread, &row.MadeInRun, &row.Content, &row.ContentDigest, &row.State)
	if err != nil && err != pgx.ErrNoRows {
		return nil, fmt.Errorf("store: put commitment: %w", err)
	}
	if err == pgx.ErrNoRows {
		// 冲突——读回既有行（重放幂等）
		const rb = `SELECT id, agent_id, made_in_thread, COALESCE(made_in_run, ''), content, content_digest, state FROM agent_commitments WHERE id = $1`
		if err := s.Pool.QueryRow(ctx, rb, in.ID).Scan(&row.ID, &row.AgentID, &row.MadeInThread, &row.MadeInRun, &row.Content, &row.ContentDigest, &row.State); err != nil {
			return nil, fmt.Errorf("store: read back commitment: %w", err)
		}
		return &row, nil
	}
	// 事件（真相——幂等 dedupe）
	_, _ = s.AppendEvent(ctx, in.MadeInThread, in.MadeInRun, event.AgentCommitted, payload, "commit:"+in.ID)
	return &row, nil
}

// ConfirmCommitSent 发送回执：pending → open（确认发出）。
func (s *Store) ConfirmCommitSent(ctx context.Context, id string) error {
	if err := commitment.CanTransition(commitment.StatePending, commitment.StateOpen); err != nil {
		_ = err
	}
	const q = `UPDATE agent_commitments SET state = 'open', confirmed_at = now()
WHERE id = $1 AND state = 'pending'`
	tag, err := s.Pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: confirm commitment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: confirm commitment 0 行（非 pending 或不存在）")
	}
	return nil
}

// TransitionCommit 状态转移（§2.2 规则——CAS 语义：WHERE state = 旧态）。
func (s *Store) TransitionCommit(ctx context.Context, id string, from, to commitment.State) (bool, error) {
	if err := commitment.CanTransition(from, to); err != nil {
		return false, err
	}
	const q = `UPDATE agent_commitments SET state = $3, resolved_at = now() WHERE id = $1 AND state = $2`
	tag, err := s.Pool.Exec(ctx, q, id, string(from), string(to))
	if err != nil {
		return false, fmt.Errorf("store: transition commitment: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListExpiredCommitments 扫 due_at 已过且 open（reconciler 骨架）。
func (s *Store) ListExpiredCommitments(ctx context.Context, now string, limit int) ([]CommitRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
SELECT id, agent_id, made_in_thread, COALESCE(made_in_run, ''), content, content_digest, state
FROM agent_commitments WHERE state = 'open' AND due_at IS NOT NULL AND due_at < $1
ORDER BY due_at LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store: expired commitments: %w", err)
	}
	defer rows.Close()
	var out []CommitRow
	for rows.Next() {
		var r CommitRow
		if err := rows.Scan(&r.ID, &r.AgentID, &r.MadeInThread, &r.MadeInRun, &r.Content, &r.ContentDigest, &r.State); err != nil {
			return nil, fmt.Errorf("store: scan commitment: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListStalePendingCommitments 扫 pending 超时（反向 reconciler——前向孤儿）。
func (s *Store) ListStalePendingCommitments(ctx context.Context, olderThan string, limit int) ([]CommitRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
SELECT id, agent_id, made_in_thread, COALESCE(made_in_run, ''), content, content_digest, state
FROM agent_commitments WHERE state = 'pending' AND made_at < $1
ORDER BY made_at LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("store: stale pending commitments: %w", err)
	}
	defer rows.Close()
	var out []CommitRow
	for rows.Next() {
		var r CommitRow
		if err := rows.Scan(&r.ID, &r.AgentID, &r.MadeInThread, &r.MadeInRun, &r.Content, &r.ContentDigest, &r.State); err != nil {
			return nil, fmt.Errorf("store: scan commitment: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
