package api

import (
	"math"
	"sync"
	"time"
)

// tokenBucket 经典令牌桶（三级限流的组件；边界语义设计 §6：429 + Retry-After）。
type tokenBucket struct {
	rate   float64 // 每秒补充令牌数
	burst  float64 // 桶容量
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64, burst int, now time.Time) *tokenBucket {
	b := &tokenBucket{rate: rate, burst: float64(burst), last: now}
	if b.burst < 1 {
		b.burst = 1
	}
	b.tokens = b.burst
	return b
}

// take 尝试取 1 枚令牌；ok=false 时 retryAfter 为下次可取的大致秒数。
func (b *tokenBucket) take(now time.Time) (ok bool, retryAfter time.Duration) {
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	need := 1 - b.tokens
	wait := time.Duration(need/b.rate*float64(time.Second)) + 1*time.Second
	return false, wait
}

// Limiter 三级限流（org/user/session 令牌桶；MVP：api 层 session 桶先行接线，
// org/user 桶已接线（期 3 §A principal 限流）——组件已支持任意 scope 键）。
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64
	burst   int
	now     func() time.Time // 可注入（测试）
}

// NewLimiter rate 为每秒补充速率，burst 为桶容量。
func NewLimiter(rate float64, burst int) *Limiter {
	return &Limiter{
		buckets: map[string]*tokenBucket{},
		rate:    rate,
		burst:   burst,
		now:     time.Now,
	}
}

// Take 对 (scope, id) 取一枚令牌。ok=false 时返回 Retry-After 秒。
func (l *Limiter) Take(scope, id string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := scope + ":" + id
	b, exists := l.buckets[key]
	if !exists {
		b = newTokenBucket(l.rate, l.burst, l.now())
		l.buckets[key] = b
	}
	return b.take(l.now())
}

// Reset 清空全部桶（测试用）。
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = map[string]*tokenBucket{}
}
