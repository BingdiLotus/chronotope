package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

func TestTokenBucketTakeAndRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := &now
	b := newTokenBucket(1.0, 2, *clock) // 1 token/s，容量 2

	for i := 0; i < 2; i++ {
		if ok, _ := b.take(*clock); !ok {
			t.Fatalf("第 %d 枚应可取（初始满桶）", i+1)
		}
	}
	ok, retry := b.take(*clock)
	if ok {
		t.Fatal("空桶应拒绝")
	}
	if retry <= 0 {
		t.Fatalf("Retry-After 应 > 0，得 %v", retry)
	}
	// 1.5s 后补 1.5 枚 → 可取 1 枚
	*clock = now.Add(1500 * time.Millisecond)
	if ok, _ := b.take(*clock); !ok {
		t.Fatal("补流后应可取")
	}
}

func TestLimiterScopedBuckets(t *testing.T) {
	now := time.Unix(2000, 0)
	l := NewLimiter(0.1, 1)
	l.now = func() time.Time { return now }

	if ok, _ := l.Take("session", "s_1"); !ok {
		t.Fatal("s_1 首枚应可取")
	}
	if ok, retry := l.Take("session", "s_1"); ok || retry <= 0 {
		t.Fatalf("s_1 第二枚应拒绝且 Retry-After>0: ok=%v retry=%v", ok, retry)
	}
	// 不同 scope 互不影响（三级限流：session 桶独立于 org/user）
	if ok, _ := l.Take("session", "s_2"); !ok {
		t.Fatal("s_2 的桶应独立")
	}
}

func TestSubmitRunDoubleOpen409(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	// 预置一条活跃 run（running）——双开场景
	fs.runs["r_active"] = &store.Run{ID: "r_active", SessionID: sessionID, Status: sessionapi.RunRunning}
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/runs",
		`{"input":"hi"}`, map[string]string{"Idempotency-Key": "double-1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("双开应 409，得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "r_active") {
		t.Fatalf("409 应携带 active_run_id: %s", rec.Body.String())
	}
}

func TestSubmitRunRateLimit429(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	// 收紧限流：容量 1、极慢补流 → 第二次提交必 429
	h.Limiter = NewLimiter(0.0001, 1)
	rec1 := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/runs",
		`{"input":"hi"}`, map[string]string{"Idempotency-Key": "rl-1"})
	if rec1.Code != http.StatusCreated {
		t.Fatalf("首次提交应 201，得 %d: %s", rec1.Code, rec1.Body.String())
	}
	rec2 := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/runs",
		`{"input":"hi"}`, map[string]string{"Idempotency-Key": "rl-2"})
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("超限应 429，得 %d: %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After 头（边界语义设计 §6）")
	}
}
