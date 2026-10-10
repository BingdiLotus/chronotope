// Package sandbox 是沙箱与执行器的行类型（期 7：从 store 下沉）。
package sandbox

import "time"

// SandboxRow 沙箱事实状态行。
type SandboxRow struct {
	SandboxID     string
	OrgID         string
	SessionID     string
	Driver        string
	ContainerRef  *string
	Image         string
	Limits        map[string]string
	Tier          int
	SnapshotRef   *string
	FileSyncState string
	Status        string
	TTL           *time.Duration
	CreatedAt     time.Time
}

// ExecutorRow 执行器注册行（heartbeat 推进）。
type ExecutorRow struct {
	ID           string    `json:"id"`
	OrgID        string    `json:"org_id"`
	Kind         string    `json:"kind"` // docker | e2b_selfhosted
	Endpoint     string    `json:"endpoint"`
	HeartbeatAt  time.Time `json:"heartbeat_at"`
	Capabilities []byte    `json:"-"`
}
