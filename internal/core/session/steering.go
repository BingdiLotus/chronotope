package session

import "time"

// Steering 是运行中转向（A4——Pi 的 follow-up inbox 对应：排队输入；
// 期 7 纪律：领域类型下沉 core）。
type Steering struct {
	ID        int64
	SessionID string
	Input     string
	CreatedAt time.Time
}
