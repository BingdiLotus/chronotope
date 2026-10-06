package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/jackc/pgx/v5"
)

// AppendEvent 幂等插入事件（append-only，契约规范 §5）。
// dedupe_key 冲突时读回既有行 seq——重放重复发射被幂等吞掉，seq 允许 gap。
// worker 是事件唯一写入者；api 只读投影（订阅分发与计量聚合）。
func (s *Store) AppendEvent(ctx context.Context, sessionID, runID string, typ event.Type, payload json.RawMessage, dedupeKey string) (seq int64, err error) {
	const insert = `
INSERT INTO events (session_id, run_id, type, payload, dedupe_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (dedupe_key) DO NOTHING
RETURNING id, seq`
	// 事件与投递入队同事务（落地方案 §5 outbox 语义）：订阅方在事件提交的
	// 同一事务内获得投递行——无两阶段问题，投递 worker 只管拉取。
	// 注意：outbox.event_id 引用 events.id（PK），而 seq 是每会话单调号——
	// 两者是不同的序列（e2e 实证曾用 seq 入队致 GetEvent 失配静默丢弃）。
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: append event begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var eventID int64
	err = tx.QueryRow(ctx, insert, sessionID, nullable(runID), string(typ), payload, dedupeKey).Scan(&eventID, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		// 重放重复发射：读回既有 seq，不再入队（幂等吞掉）
		const readBack = `SELECT seq FROM events WHERE dedupe_key = $1`
		if err = tx.QueryRow(ctx, readBack, dedupeKey).Scan(&seq); err != nil {
			return 0, fmt.Errorf("store: read back event %q: %w", dedupeKey, err)
		}
		return seq, tx.Commit(ctx)
	}
	if err != nil {
		return 0, fmt.Errorf("store: append event %q: %w", dedupeKey, err)
	}
	// 订阅入队（同事务）
	if _, err := tx.Exec(ctx, `
INSERT INTO outbox (session_id, event_id, url)
SELECT $1, $2, target FROM subscriptions WHERE session_id = $1`, sessionID, eventID); err != nil {
		return 0, fmt.Errorf("store: enqueue deliveries: %w", err)
	}
	return seq, tx.Commit(ctx)
}

// EventRow 是 events 表的查询结果行。
type EventRow struct {
	ID        int64
	SessionID string
	RunID     string
	Seq       int64
	Type      event.Type
	Payload   json.RawMessage
	At        time.Time
}

// ListEvents 按 seq 续读会话事件（契约规范 §2：GET /events?after=seq&limit；允许 gap）。
func (s *Store) ListEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]EventRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	const q = `
SELECT id, session_id, COALESCE(run_id, ''), seq, type, payload, at
FROM events
WHERE session_id = $1 AND seq > $2
ORDER BY seq
LIMIT $3`
	rows, err := s.Pool.Query(ctx, q, sessionID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.ID, &e.SessionID, &e.RunID, &e.Seq, &e.Type, &e.Payload, &e.At); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
