package events

import (
	"context"
	"testing"
	"time"
)

func TestHubPublishDeliversHints(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(context.Background(), "s_1", 0)
	defer h.Unsubscribe(sub)

	h.Publish("s_1", 1)
	h.Publish("s_1", 3) // seq 允许 gap

	select {
	case seq := <-sub.C():
		if seq != 1 {
			t.Fatalf("首个提示应为 seq=1，得 %d", seq)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到提示")
	}
	select {
	case seq := <-sub.C():
		if seq != 3 {
			t.Fatalf("第二个提示应为 seq=3（允许 gap），得 %d", seq)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到第二个提示")
	}
	if got := h.LastPublished("s_1"); got != 3 {
		t.Fatalf("水位应为 3，得 %d", got)
	}
}

func TestHubAfterCursorFilters(t *testing.T) {
	h := NewHub()
	// after=2：seq<=2 的提示不投递（断线重连续读语义）
	sub := h.Subscribe(context.Background(), "s_1", 2)
	defer h.Unsubscribe(sub)

	h.Publish("s_1", 1)
	h.Publish("s_1", 2)
	h.Publish("s_1", 3)

	select {
	case seq := <-sub.C():
		if seq != 3 {
			t.Fatalf("after=2 只应收 seq=3，得 %d", seq)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到 seq=3 提示")
	}
}

func TestHubSessionIDsAndUnsubscribe(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(context.Background(), "s_1", 0)
	ids := h.SessionIDs()
	if len(ids) != 1 || ids[0] != "s_1" {
		t.Fatalf("SessionIDs 应含 s_1: %v", ids)
	}
	h.Unsubscribe(sub)
	// 异步移除：等待收敛
	deadline := time.Now().Add(time.Second)
	for len(h.SessionIDs()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.SessionIDs(); len(got) != 0 {
		t.Fatalf("退订后 SessionIDs 应为空: %v", got)
	}
}

func TestHubBackpressureDropsHint(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(context.Background(), "s_1", 0)
	defer h.Unsubscribe(sub)

	// 灌满缓冲（cap=256）后继续发布：不阻塞、不 panic（提示丢弃，客户端对账兜底）
	for i := int64(1); i <= 300; i++ {
		h.Publish("s_1", i)
	}
	if got := h.LastPublished("s_1"); got != 300 {
		t.Fatalf("水位应到 300，得 %d", got)
	}
}
