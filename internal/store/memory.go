package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/memory"

	"github.com/jackc/pgx/v5"
)

// Summary 是 summaries 行（期 7 下沉：core/memory）。
type Summary = memory.Summary

// MemoryItem 是 memory_items 行（期 7 下沉：core/memory）。
type MemoryItem = memory.MemoryItem

// HashContent 内容哈希（去重键）。
func HashContent(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// LatestSummary 取会话某 topic 的最新摘要（无则 ErrNotFound）。
func (s *Store) LatestSummary(ctx context.Context, sessionID, topic string) (*Summary, error) {
	const q = `
SELECT session_id, topic, version, summary, diff, COALESCE(created_by_run, ''), created_at
FROM summaries WHERE session_id = $1 AND topic = $2
ORDER BY version DESC LIMIT 1`
	var sum Summary
	err := s.Pool.QueryRow(ctx, q, sessionID, topic).Scan(
		&sum.SessionID, &sum.Topic, &sum.Version, &sum.Summary, &sum.Diff, &sum.CreatedByRun, &sum.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: latest summary: %w", err)
	}
	return &sum, nil
}

// CreateSummary 写摘要（幂等：同 (session, topic, version) 冲突即已存在，返回 false）。
func (s *Store) CreateSummary(ctx context.Context, sum Summary) (bool, error) {
	const q = `
INSERT INTO summaries (session_id, topic, version, summary, diff, created_by_run)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (session_id, topic, version) DO NOTHING`
	tag, err := s.Pool.Exec(ctx, q, sum.SessionID, sum.Topic, sum.Version, sum.Summary, sum.Diff, sum.CreatedByRun)
	if err != nil {
		return false, fmt.Errorf("store: create summary: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CreateMemoryItem 写记忆条目（内容哈希去重：冲突返回 false）。
func (s *Store) CreateMemoryItem(ctx context.Context, item MemoryItem) (bool, error) {
	if item.ContentHash == "" {
		item.ContentHash = HashContent(item.Content)
	}
	const q = `
INSERT INTO memory_items (session_id, topic, kind, content, content_hash, source_run_id, source_step, version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (session_id, topic, content_hash) DO NOTHING`
	tag, err := s.Pool.Exec(ctx, q, item.SessionID, item.Topic, item.Kind, item.Content, item.ContentHash,
		item.SourceRunID, item.SourceStep, item.Version)
	if err != nil {
		return false, fmt.Errorf("store: create memory item: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListMemoryItems 检索条目：topic 作用域优先（MVP 无嵌入，按 recency 排序；pgvector 后置）。
// topic 为空时返回全部。
func (s *Store) ListMemoryItems(ctx context.Context, sessionID, topic string, limit int) ([]MemoryItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	const q = `
SELECT session_id, topic, kind, content, content_hash, COALESCE(source_run_id, ''), source_step, version, created_at
FROM memory_items
WHERE session_id = $1 AND ($2 = '' OR topic = $2)
ORDER BY created_at DESC, id DESC LIMIT $3`
	rows, err := s.Pool.Query(ctx, q, sessionID, topic, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list memory items: %w", err)
	}
	defer rows.Close()
	var out []MemoryItem
	for rows.Next() {
		var it MemoryItem
		if err := rows.Scan(&it.SessionID, &it.Topic, &it.Kind, &it.Content, &it.ContentHash,
			&it.SourceRunID, &it.SourceStep, &it.Version, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan memory item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ListSummaries 会话全部摘要（api 控制面用）。
func (s *Store) ListSummaries(ctx context.Context, sessionID string) ([]Summary, error) {
	const q = `
SELECT session_id, topic, version, summary, diff, COALESCE(created_by_run, ''), created_at
FROM summaries WHERE session_id = $1 ORDER BY topic, version`
	rows, err := s.Pool.Query(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list summaries: %w", err)
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var sum Summary
		if err := rows.Scan(&sum.SessionID, &sum.Topic, &sum.Version, &sum.Summary, &sum.Diff,
			&sum.CreatedByRun, &sum.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan summary: %w", err)
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}
