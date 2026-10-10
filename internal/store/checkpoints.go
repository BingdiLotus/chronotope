package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/checkpoint"
	"github.com/bingdilotus/chronotope/internal/core/session"

	"github.com/jackc/pgx/v5"
)

// Checkpoint 是会话时间坐标（时间旅行 期 2：seq 指针 + 沙箱快照引用）。
// 期 7 下沉：定义在 core/checkpoint——store 用别名（只做 SQL）。
type Checkpoint = checkpoint.Checkpoint

// CreateCheckpoint 落库时间坐标（幂等：同 id 已存在返回 false）。
func (s *Store) CreateCheckpoint(ctx context.Context, cp Checkpoint) (bool, error) {
	// 消息水位由库内计算（调用方不传——checkpoint 语义 =「此刻的投影水位」）
	const q = `
INSERT INTO checkpoints (id, session_id, seq, snapshot_ref, max_message_id)
VALUES ($1, $2, $3, $4,
  COALESCE((SELECT max(id) FROM messages WHERE session_id = $2), 0))
ON CONFLICT (id) DO NOTHING`
	tag, err := s.Pool.Exec(ctx, q, cp.ID, cp.SessionID, cp.Seq, cp.SnapshotRef)
	if err != nil {
		return false, fmt.Errorf("store: create checkpoint: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// GetCheckpoint 读时间坐标。
func (s *Store) GetCheckpoint(ctx context.Context, id string) (*Checkpoint, error) {
	const q = `SELECT id, session_id, seq, snapshot_ref, max_message_id, created_at FROM checkpoints WHERE id = $1`
	var cp Checkpoint
	err := s.Pool.QueryRow(ctx, q, id).Scan(&cp.ID, &cp.SessionID, &cp.Seq, &cp.SnapshotRef, &cp.MaxMessageID, &cp.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get checkpoint: %w", err)
	}
	return &cp, nil
}

// ListCheckpoints 会话的时间坐标升序。
func (s *Store) ListCheckpoints(ctx context.Context, sessionID string, limit int) ([]Checkpoint, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	const q = `SELECT id, session_id, seq, snapshot_ref, max_message_id, created_at FROM checkpoints WHERE session_id = $1 ORDER BY seq LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list checkpoints: %w", err)
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		var cp Checkpoint
		if err := rows.Scan(&cp.ID, &cp.SessionID, &cp.Seq, &cp.SnapshotRef, &cp.MaxMessageID, &cp.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan checkpoint: %w", err)
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

// ForkSession 从时间坐标派生新会话（写时复制——MVP 真实复制事件+消息前缀；
// 事件量大时演进为「父指针 + 合流读取」，见正式版架构 §4.2）。返回新会话 id。
func (s *Store) ForkSession(ctx context.Context, newSessionID, parentSessionID string, atSeq int64, atCheckpoint string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: fork begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// 会话行：血缘 + 复制 parent 的 org/agent 归属
	var orgID, agentID string
	if err := tx.QueryRow(ctx,
		`SELECT org_id, agent_id FROM sessions WHERE id = $1`, parentSessionID).
		Scan(&orgID, &agentID); err != nil {
		return fmt.Errorf("store: fork parent: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO sessions (id, org_id, agent_id, forked_from_session, forked_at_seq, status)
VALUES ($1, $2, $3, $4, $5, 'ready')`, newSessionID, orgID, agentID, parentSessionID, atSeq); err != nil {
		return fmt.Errorf("store: fork session row: %w", err)
	}
	// 事件前缀复制（seq 重新编号自 1——新会话独立时间轴；dedupe_key 置空：
	// 复制的事件是历史事实，不再接受幂等重放写入——且全局唯一键会与父分支
	// 冲突（集成测试实证 duplicate key））
	if _, err := tx.Exec(ctx, `
INSERT INTO events (session_id, run_id, seq, type, payload, dedupe_key, at)
SELECT $1, run_id, seq, type, payload, NULL, at
FROM events WHERE session_id = $2 AND seq <= $3 ORDER BY seq`, newSessionID, parentSessionID, atSeq); err != nil {
		return fmt.Errorf("store: fork events: %w", err)
	}
	// 消息前缀复制（对话真相的投影；id 重置）
	if _, err := tx.Exec(ctx, `
INSERT INTO messages (session_id, run_id, step, role, content, created_at)
SELECT $1, run_id, step, role, content, created_at
FROM messages WHERE session_id = $2 ORDER BY id`, newSessionID, parentSessionID); err != nil {
		return fmt.Errorf("store: fork messages: %w", err)
	}
	// 工作区索引复制（期 2 遗留 #1：fork 空间面写时复制——分支会话的沙箱从
	// 内容寻址恢复（ensureSandbox 无快照有索引 → blob: 触发）；对象键
	// org/blobs/{hash} 跨会话共享（org 级 dedup 语义，无需复制对象本体）
	if _, err := tx.Exec(ctx, `
INSERT INTO workspace_files (session_id, path, hash, size)
SELECT $1, path, hash, size
FROM workspace_files WHERE session_id = $2`, newSessionID, parentSessionID); err != nil {
		return fmt.Errorf("store: fork workspace files: %w", err)
	}
	// 派生事件（fork 事件写入**新会话**时间轴——父会话时间轴不可变）
	if _, err := tx.Exec(ctx, `
INSERT INTO events (session_id, run_id, type, payload, dedupe_key)
VALUES ($1, '', 'session.forked', $2, NULL)`, newSessionID, json.RawMessage(fmt.Sprintf(
		`{"parent_session_id":%q,"at_seq":%d,"checkpoint_id":%q}`, parentSessionID, atSeq, atCheckpoint))); err != nil {
		return fmt.Errorf("store: fork event: %w", err)
	}
	return tx.Commit(ctx)
}

// RollbackSession 回退到时间坐标：消息投影截断到 checkpoint 时点（事件轴
// 保留 rollback 事件为审计——真相不可变，投影重建）；会话状态回 ready。
func (s *Store) RollbackSession(ctx context.Context, sessionID string, cp *Checkpoint) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: rollback begin: %w", err)
	}
	defer tx.Rollback(ctx)
	// 投影截断：删除消息水位之后的行（消息是投影；真相在事件轴——rollback
	// 事件追加在事件轴，审计可还原「发生了什么 + 何时回退」）
	if _, err := tx.Exec(ctx, `
DELETE FROM messages WHERE session_id = $1 AND id > $2`, sessionID, cp.MaxMessageID); err != nil {
		return fmt.Errorf("store: rollback messages: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET status = 'ready' WHERE id = $1`, sessionID); err != nil {
		return fmt.Errorf("store: rollback status: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO events (session_id, run_id, type, payload, dedupe_key)
VALUES ($1, '', 'session.rolled_back', $2, NULL)`, sessionID, json.RawMessage(fmt.Sprintf(
		`{"checkpoint_id":%q,"at_seq":%d,"snapshot_ref":%q}`, cp.ID, cp.Seq, cp.SnapshotRef))); err != nil {
		return fmt.Errorf("store: rollback event: %w", err)
	}
	return tx.Commit(ctx)
}

// SessionDiff 是两会话时间轴的差集：公共前缀长度 + 各自后缀事件。
// SessionDiff（期 7 下沉：core/session）。
type SessionDiff = session.SessionDiff

// DiffSessions 事件差集（时间旅行 期 2：分支对比）。
func (s *Store) DiffSessions(ctx context.Context, a, b string, limit int) (*SessionDiff, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	evA, err := s.ListEvents(ctx, a, 0, limit)
	if err != nil {
		return nil, fmt.Errorf("store: diff A: %w", err)
	}
	evB, err := s.ListEvents(ctx, b, 0, limit)
	if err != nil {
		return nil, fmt.Errorf("store: diff B: %w", err)
	}
	diff := &SessionDiff{}
	n := len(evA)
	if len(evB) < n {
		n = len(evB)
	}
	i := 0
	for i < n && evA[i].Type == evB[i].Type && string(evA[i].Payload) == string(evB[i].Payload) {
		i++
	}
	diff.CommonPrefix = int64(i)
	diff.OnlyA = evA[i:]
	diff.OnlyB = evB[i:]
	return diff, nil
}
