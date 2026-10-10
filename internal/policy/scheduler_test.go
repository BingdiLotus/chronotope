package policy

import (
	"context"
	"encoding/json"
	"fmt"
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
	// 无等待 → 显式 schedule 到点即 run（quiet 后备只用于周期轮询场景——
	// 不误伤显式排程）
	w2 := WaitHandleDecide{
		ActiveWaits: func(context.Context, string) ([]map[string]any, error) { return nil, nil },
	}
	if d := w2.Decide(context.Background(), "s1"); d.Hint != store.HintRun {
		t.Fatalf("无活跃等待应 run: %+v", d)
	}
}

// TestWaitHandleDecidePickDelegation 期 7：Pick 委托 RoundRobin（接口完整性
// ——SchedulerPolicy 的装箱选择与判定分离）。
func TestWaitHandleDecidePickDelegation(t *testing.T) {
	w := WaitHandleDecide{}
	cands := []ExecutorCandidate{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	// RoundRobin 是同会话哈希稳定位（非真轮转——重放确定性要求）
	first := w.Pick(context.Background(), "s1", cands)
	if first.ID == "" {
		t.Fatalf("候选选择不得为空")
	}
	for i := 0; i < 20; i++ {
		if p := w.Pick(context.Background(), "s1", cands); p.ID != first.ID {
			t.Fatalf("同会话应稳定: %s != %s", p.ID, first.ID)
		}
	}
	// 不同会话分布（哈希打散）
	seen := map[string]bool{}
	for _, sid := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		seen[w.Pick(context.Background(), sid, cands).ID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("不同会话应打散: %v", seen)
	}
}

// TestWaitHandleDecideEmptyCandidates 空候选（无 executor 注册——不 panic）。
func TestWaitHandleDecideEmptyCandidates(t *testing.T) {
	w := WaitHandleDecide{}
	if p := w.Pick(context.Background(), "s1", nil); p.ID != "" {
		t.Fatalf("空候选应返回零值: %+v", p)
	}
}

// TestQuietWhenIdleActiveRunError 活跃 run 查询失败（fail-open 语义——错误
// 时不应判定 quiet 而应 run——可判定性的保守侧）。
func TestQuietWhenIdleActiveRunError(t *testing.T) {
	q := QuietWhenIdle{
		HasActiveRun: func(context.Context, string) (bool, error) { return false, fmt.Errorf("db down") },
	}
	if d := q.Decide(context.Background(), "s1"); d.Hint != store.HintRun {
		t.Fatalf("活跃查询失败应保守 run: %+v", d)
	}
}

// TestDecisionSerialization journaled 判定的序列化（scheduler.decide 事件载荷）。
func TestDecisionSerialization(t *testing.T) {
	d := store.Decision{Hint: store.HintWait, Reason: "waiting: timer"}
	b := d.DecisionJSON()
	var back struct {
		Hint   string `json:"hint"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(b, &back); err != nil || back.Hint != "wait" || back.Reason == "" {
		t.Fatalf("Decision 序列化: %s %+v err=%v", b, back, err)
	}
}
