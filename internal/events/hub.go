// Package events 是事件投影层（落地方案 §1）：SSE hub、outbox 投递。
//
// 分发策略（落地方案 §5）：PG LISTEN/NOTIFY 只发 (session_id, seq) 提示（payload
// <8KB 限制），api 收到提示后回查 events 表再推给订阅者；心跳 + 客户端定期以
// after=seq 对账兜底静默丢失；seq 允许 gap，订阅端必须容忍。
package events

import (
	"context"
	"sync"
)

// Subscription 是单个客户端的时间轴订阅（session 作用域 + after 游标）。
type Subscription struct {
	sessionID string
	after     int64 // 断线重连续读游标（契约规范 §2：GET /events?after=seq）
	ch        chan int64
	ctx       context.Context
	cancel    context.CancelFunc
}

// C 是提示通道：收到 seq 提示后按 (session_id, seq) 回查 store 取完整事件再推送。
// 骨架阶段为内存实现；生产接 LISTEN/NOTIFY（见包注释）。
func (s *Subscription) C() <-chan int64 { return s.ch }

// SessionID 返回订阅所属会话。
func (s *Subscription) SessionID() string { return s.sessionID }

// Hub 是进程内订阅表。api 多实例部署时按 session 路由或共享 NOTIFY 通道。
type Hub struct {
	mu        sync.RWMutex
	subs      map[string]map[int64]*Subscription
	published map[string]int64 // 每 session 最近一次发布的 seq（poller 水位）
	next      int64
}

// NewHub 创建空 hub。
func NewHub() *Hub {
	return &Hub{
		subs:      make(map[string]map[int64]*Subscription),
		published: make(map[string]int64),
	}
}

// SessionIDs 返回当前有订阅者的 session 列表（api poller 的轮询范围）。
func (h *Hub) SessionIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.subs))
	for id := range h.subs {
		out = append(out, id)
	}
	return out
}

// LastPublished 返回某 session 最近一次发布的 seq 水位（0 表示从未发布）。
func (h *Hub) LastPublished(sessionID string) int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.published[sessionID]
}

// Subscribe 订阅某 session 的 seq 提示流（after 为续读游标，0 表示从头）。
func (h *Hub) Subscribe(ctx context.Context, sessionID string, after int64) *Subscription {
	ctx, cancel := context.WithCancel(ctx)
	sub := &Subscription{
		sessionID: sessionID,
		after:     after,
		ch:        make(chan int64, 256),
		ctx:       ctx,
		cancel:    cancel,
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	subs, ok := h.subs[sessionID]
	if !ok {
		subs = make(map[int64]*Subscription)
		h.subs[sessionID] = subs
	}
	id := h.next
	h.next++
	subs[id] = sub

	go func() {
		<-ctx.Done()
		h.remove(sessionID, id)
	}()
	return sub
}

// Unsubscribe 主动取消订阅。
func (h *Hub) Unsubscribe(sub *Subscription) { sub.cancel() }

// Publish 向订阅者广播「新事件提示」（seq 为游标），并推进该 session 的水位。
// TODO(W1+)：接 PG LISTEN/NOTIFY 通道后，本方法由 notify handler 调用。
func (h *Hub) Publish(sessionID string, seq int64) {
	h.mu.Lock()
	if h.published[sessionID] < seq {
		h.published[sessionID] = seq
	}
	subs := h.subs[sessionID]
	h.mu.Unlock()
	for _, sub := range subs {
		if sub.after >= seq {
			continue
		}
		select {
		case sub.ch <- seq:
		default: // 背压：丢弃提示，客户端按 after=seq 对账兜底
		}
	}
}

func (h *Hub) remove(sessionID string, id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs[sessionID], id)
	if len(h.subs[sessionID]) == 0 {
		delete(h.subs, sessionID)
	}
}
