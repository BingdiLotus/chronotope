// Package checkpoint 是时间旅行的领域类型（期 7：从 store 下沉——store 只做
// SQL 与映射；kernel 的编译期契约在 core）。
package checkpoint

import "time"

// Checkpoint 时间坐标（checkpoint 树节点）。
type Checkpoint struct {
	ID           string    `json:"id"`
	SessionID    string    `json:"session_id"`
	Seq          int64     `json:"seq"`
	SnapshotRef  string    `json:"snapshot_ref"`
	MaxMessageID int64     `json:"max_message_id"` // 消息投影水位（rollback 截断依据）
	CreatedAt    time.Time `json:"created_at"`
}
