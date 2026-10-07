package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Archive 是冷层归档清单行（期 2 §B）。
type Archive struct {
	SessionID     string    `json:"session_id"`
	BucketPath    string    `json:"bucket_path"`
	EventsCount   int64     `json:"events_count"`
	MessagesCount int64     `json:"messages_count"`
	CreatedAt     time.Time `json:"created_at"`
}

// CreateArchive 落清单行（幂等：同会话覆盖）。
func (s *Store) CreateArchive(ctx context.Context, a Archive) error {
	const q = `
INSERT INTO archives (session_id, bucket_path, events_count, messages_count)
VALUES ($1, $2, $3, $4)
ON CONFLICT (session_id) DO UPDATE
SET bucket_path = EXCLUDED.bucket_path,
    events_count = EXCLUDED.events_count,
    messages_count = EXCLUDED.messages_count,
    created_at = now()`
	if _, err := s.Pool.Exec(ctx, q, a.SessionID, a.BucketPath, a.EventsCount, a.MessagesCount); err != nil {
		return fmt.Errorf("store: create archive: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, a.SessionID); err != nil {
		return fmt.Errorf("store: mark archived: %w", err)
	}
	return nil
}

// GetArchive 读清单行。
func (s *Store) GetArchive(ctx context.Context, sessionID string) (*Archive, error) {
	const q = `SELECT session_id, bucket_path, events_count, messages_count, created_at FROM archives WHERE session_id = $1`
	var a Archive
	err := s.Pool.QueryRow(ctx, q, sessionID).Scan(&a.SessionID, &a.BucketPath, &a.EventsCount, &a.MessagesCount, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get archive: %w", err)
	}
	return &a, nil
}

// SessionLastEventAt 会话最后事件时间（归档年龄判断；无事件 → 零值）。
func (s *Store) SessionLastEventAt(ctx context.Context, sessionID string) (time.Time, error) {
	const q = `SELECT COALESCE(max(at), 'epoch') FROM events WHERE session_id = $1`
	var at time.Time
	if err := s.Pool.QueryRow(ctx, q, sessionID).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: session last event at: %w", err)
	}
	return at, nil
}
