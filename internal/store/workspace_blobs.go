package store

import (
	"context"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/memory"
)

// WorkspaceFile 是工作区文件索引行（path → 内容寻址；期 2 §A）。
// WorkspaceFile 工作区文件索引行（期 7 下沉：core/memory）。
type WorkspaceFile = memory.WorkspaceFile

// UpsertWorkspaceFile 写索引行（同路径覆盖；内容寻址幂等——同 hash 对象复用）。
func (s *Store) UpsertWorkspaceFile(ctx context.Context, f WorkspaceFile) error {
	const q = `
INSERT INTO workspace_files (session_id, path, hash, size)
VALUES ($1, $2, $3, $4)
ON CONFLICT (session_id, path) DO UPDATE
SET hash = EXCLUDED.hash, size = EXCLUDED.size, updated_at = now()`
	if _, err := s.Pool.Exec(ctx, q, f.SessionID, f.Path, f.Hash, f.Size); err != nil {
		return fmt.Errorf("store: upsert workspace file: %w", err)
	}
	return nil
}

// ListWorkspaceFiles 读会话的目录索引（blob 恢复的清单）。
func (s *Store) ListWorkspaceFiles(ctx context.Context, sessionID string, limit int) ([]WorkspaceFile, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	const q = `SELECT session_id, path, hash, size, updated_at FROM workspace_files WHERE session_id = $1 ORDER BY path LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list workspace files: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceFile
	for rows.Next() {
		var f WorkspaceFile
		if err := rows.Scan(&f.SessionID, &f.Path, &f.Hash, &f.Size, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan workspace file: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
