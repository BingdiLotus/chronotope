package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bingdilotus/chronotope/internal/core/extension"
)

// RegisterExtension 注册扩展包（同 id 覆盖 definition/version）。
func (s *Store) RegisterExtension(ctx context.Context, ext extension.Definition) error {
	defJSON, _ := json.Marshal(ext)
	const q = `
INSERT INTO extensions (id, org_id, name, kind, definition, version, state)
VALUES ($1, $2, $3, $4, $5, $6, 'active')
ON CONFLICT (id) DO UPDATE
SET definition = EXCLUDED.definition, version = EXCLUDED.version, state = 'active'`
	if _, err := s.Pool.Exec(ctx, q, ext.ID, "", ext.Name, ext.Kind, defJSON, ext.Version); err != nil {
		return fmt.Errorf("store: register extension: %w", err)
	}
	return nil
}

// ListExtensions org 的 active 扩展包。
func (s *Store) ListExtensions(ctx context.Context, orgID string) ([]extension.Definition, error) {
	rows, err := s.Pool.Query(ctx, `SELECT definition::text FROM extensions WHERE org_id = $1 AND state = 'active' ORDER BY name`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list extensions: %w", err)
	}
	defer rows.Close()
	var out []extension.Definition
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("store: scan extension: %w", err)
		}
		var ext extension.Definition
		if err := json.Unmarshal([]byte(raw), &ext); err == nil {
			out = append(out, ext)
		}
	}
	return out, rows.Err()
}
