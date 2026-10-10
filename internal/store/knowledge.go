package store

import (
	"context"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/memory"
)

// KnowledgeItem 是共享知识条目（期 7 下沉：core/memory）。
type KnowledgeItem = memory.KnowledgeItem

// CreateKnowledge 幂等写入（同 id 冲突忽略）。
func (s *Store) CreateKnowledge(ctx context.Context, k KnowledgeItem) error {
	const q = `
INSERT INTO tenant_knowledge (id, tenant_id, content, embedding, source_session)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, k.ID, k.TenantID, k.Content, vecStr(k.Embedding), k.SourceSession); err != nil {
		return fmt.Errorf("store: create knowledge: %w", err)
	}
	return nil
}

// RetrieveKnowledge 余弦相似度 topK 检索（pgvector <=> 距离排序）。
func (s *Store) RetrieveKnowledge(ctx context.Context, tenantID string, embedding []float32, topK int) ([]KnowledgeItem, error) {
	if topK <= 0 || topK > 20 {
		topK = 3
	}
	const q = `
SELECT id, tenant_id, content, source_session, created_at
FROM tenant_knowledge
WHERE tenant_id = $1
ORDER BY embedding <=> $2
LIMIT $3`
	rows, err := s.Pool.Query(ctx, q, tenantID, vecStr(embedding), topK)
	if err != nil {
		return nil, fmt.Errorf("store: retrieve knowledge: %w", err)
	}
	defer rows.Close()
	var out []KnowledgeItem
	for rows.Next() {
		var k KnowledgeItem
		if err := rows.Scan(&k.ID, &k.TenantID, &k.Content, &k.SourceSession, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan knowledge: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// vecStr 将 float32 向量转 pgvector 字面量（[0.1,0.2,...]）。
func vecStr(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	out := fmt.Sprintf("[%v", v[0])
	for _, x := range v[1:] {
		out += fmt.Sprintf(",%v", x)
	}
	return out + "]"
}
