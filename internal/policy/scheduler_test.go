package policy

import (
	"context"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/store"
)

// TestQuietWhenIdle M3（两必答之一：internal/policy 补测试——层 1 策略缝的
// 治理能力必须有测试）。
func TestQuietWhenIdle(t *testing.T) {
	// 活跃 run → wait
	q := QuietWhenIdle{
		HasActiveRun: func(context.Context, string) (bool, error) { return true, nil },
	}
	if d := q.Decide(context.Background(), "s1"); d.Hint != store.HintWait {
		t.Fatalf("活跃 run 应 wait: %+v", d)
	}
	// 无活跃 + 10 分钟无新证据 → quiet（空转关闭）
	q2 := QuietWhenIdle{
		HasActiveRun:    func(context.Context, string) (bool, error) { return false, nil },
		RecentCompleted: func(context.Context, string, time.Duration) (bool, error) { return false, nil },
	}
	if d := q2.Decide(context.Background(), "s1"); d.Hint != store.HintQuiet {
		t.Fatalf("无新证据应 quiet: %+v", d)
	}
	// 证据新鲜 → run
	q3 := QuietWhenIdle{
		HasActiveRun:    func(context.Context, string) (bool, error) { return false, nil },
		RecentCompleted: func(context.Context, string, time.Duration) (bool, error) { return true, nil },
	}
	if d := q3.Decide(context.Background(), "s1"); d.Hint != store.HintRun {
		t.Fatalf("证据新鲜应 run: %+v", d)
	}
}

// TestWaitHandleDecide 期 6 ①：可判定 DECIDE——有活跃等待（在等 CI/审批）
// 的会话判 wait 非 quiet（QuietWhenIdle 启发式会误判的空转关闭）。
func TestWaitHandleDecide(t *testing.T) {
	w := WaitHandleDecide{
		ActiveWaits: func(context.Context, string) ([]map[string]any, error) {
			return []map[string]any{{"kind": "operator_input", "intent": "审批请求", "expect": "approve|reject"}}, nil
		},
	}
	if d := w.Decide(context.Background(), "s1"); d.Hint != store.HintWait {
		t.Fatalf("有活跃等待应 wait 非 quiet: %+v", d)
	}
	// 无等待 → 启发式（10 分钟无证据 → quiet）
	w2 := WaitHandleDecide{
		ActiveWaits:     func(context.Context, string) ([]map[string]any, error) { return nil, nil },
		RecentCompleted: func(context.Context, string, time.Duration) (bool, error) { return false, nil },
	}
	if d := w2.Decide(context.Background(), "s1"); d.Hint != store.HintQuiet {
		t.Fatalf("无等待且无新证据应 quiet: %+v", d)
	}
}
