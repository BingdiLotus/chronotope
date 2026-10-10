// Package memory 是分层记忆与投影的领域类型（期 7：从 store 下沉）。
package memory

import (
	"encoding/json"
	"time"
)

// Summary 是 summaries 行（主题滚动摘要；派生数据，可重算）。
type Summary struct {
	SessionID    string
	Topic        string
	Version      int
	Summary      string
	Diff         string
	CreatedByRun string
	CreatedAt    time.Time
}

// MemoryItem 是 memory_items 行（长期记忆条目；派生数据，带来源引用）。
type MemoryItem struct {
	SessionID   string
	Topic       string
	Kind        string
	Content     string
	ContentHash string
	SourceRunID string
	SourceStep  int
	Version     int
	CreatedAt   time.Time
}

// Message 消息投影行。
type Message struct {
	ID        int64
	SessionID string
	RunID     string
	Step      int
	Role      string
	Content   json.RawMessage
}

// WorkspaceFile 工作区文件索引行（内容寻址）。
type WorkspaceFile struct {
	SessionID string    `json:"session_id"`
	Path      string    `json:"path"`
	Hash      string    `json:"hash"`
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updated_at"`
}

// KnowledgeItem 共享知识条目（tenant 级 pgvector；期 3 §D 基础数据服务）。
type KnowledgeItem struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	Content       string    `json:"content"`
	Embedding     []float32 `json:"-"`
	SourceSession string    `json:"source_session"`
	CreatedAt     time.Time `json:"created_at"`
}
