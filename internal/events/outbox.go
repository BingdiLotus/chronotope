package events

import (
	"context"
	"time"
)

// Outbox 投递（落地方案 §5）：events 与 outbox 同事务写入；投递 worker 定时拉取 +
// 指数退避重试。只定义接口与重试策略，实现随 W2–W3（通知与交付，W8）。
type Delivery struct {
	ID        int64
	SessionID string
	EventID   int64
	URL       string
	Attempts  int
}

// Sender 是投递实现（webhook 等）。
type Sender interface {
	Send(ctx context.Context, d Delivery) error
}

// maxAttempts 是最大重试次数。
const maxAttempts = 8

// NextAttempt 计算第 attempts 次失败后的下次投递时间：指数退避 5s×2^n，封顶 1h。
func NextAttempt(attempts int, now time.Time) time.Time {
	backoff := 5 * time.Second
	for i := 0; i < attempts && backoff < time.Hour; i++ {
		backoff *= 2
	}
	if backoff > time.Hour {
		backoff = time.Hour
	}
	return now.Add(backoff)
}
