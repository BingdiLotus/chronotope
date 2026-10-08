package restate

import (
	"context"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/execproto"
	"github.com/bingdilotus/chronotope/internal/policy"
	"github.com/bingdilotus/chronotope/internal/store"
)

type poolFakeExec struct {
	id       string
	created  int
	executed int
}

func (f *poolFakeExec) CreateSandbox(context.Context, execproto.CreateSandboxRequest) (string, error) {
	f.created++
	return "sb_" + f.id + "_" + itoa(f.created), nil
}
func (f *poolFakeExec) Execute(context.Context, string, string, string, string) (*ExecResult, error) {
	f.executed++
	return &ExecResult{Exit: 0}, nil
}
func (f *poolFakeExec) ReadFile(context.Context, string, string) (string, error) { return "", nil }
func (f *poolFakeExec) WriteFile(context.Context, string, string, string) error  { return nil }
func (f *poolFakeExec) AcquireLease(context.Context, string, string, string) (int64, error) {
	return 1, nil
}
func (f *poolFakeExec) ReleaseLease(context.Context, string, int64) error { return nil }
func (f *poolFakeExec) Snapshot(context.Context, string) (string, error)  { return "", nil }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestExecutorPoolRouting 期 4 §B：新会话策略选择 + 归属路由 + 单点兜底。
func TestExecutorPoolRouting(t *testing.T) {
	execA := &poolFakeExec{id: "A"}
	execB := &poolFakeExec{id: "B"}
	pool := NewExecutorPool(&poolFakeExec{id: "fallback"}, policy.RoundRobin{})
	pool.ListExecutors = func(context.Context) ([]store.ExecutorRow, error) {
		return []store.ExecutorRow{
			{ID: "A", Endpoint: "http://a", HeartbeatAt: time.Now()},
			{ID: "B", Endpoint: "http://b", HeartbeatAt: time.Now()},
		}, nil
	}
	owned := map[string]string{}
	pool.SandboxOwner = func(_ context.Context, sandboxID string) (string, error) { return owned[sandboxID], nil }
	pool.SetSandboxOwner = func(_ context.Context, sandboxID, owner string) error {
		owned[sandboxID] = owner
		return nil
	}
	// 注入客户端映射（clientFor 惰性建——直接预置）
	pool.setClient("A", execA)
	pool.setClient("B", execB)

	id1, err := pool.CreateSandbox(context.Background(), execproto.CreateSandboxRequest{SessionID: "s_1"})
	if err != nil || id1 == "" {
		t.Fatalf("create1: %v", err)
	}
	if owned[id1] == "" {
		t.Fatal("沙箱应落归属")
	}
	// 归属路由：Execute 走属主
	if _, err := pool.Execute(context.Background(), id1, "bash", "ls", "k1"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	ownerExec := execA
	if owned[id1] == "B" {
		ownerExec = execB
	}
	if ownerExec.executed != 1 {
		t.Fatalf("属主应执行 1 次: %+v", map[string]int{"A": execA.executed, "B": execB.executed})
	}
	// 负载分布：多会话创建 → 两 executor 均有分配（RoundRobin 哈希分布）
	for i := 0; i < 20; i++ {
		if _, err := pool.CreateSandbox(context.Background(), execproto.CreateSandboxRequest{SessionID: "s_multi_" + itoa(i)}); err != nil {
			t.Fatalf("create multi: %v", err)
		}
	}
	if execA.created == 0 || execB.created == 0 {
		t.Fatalf("负载应分布到两 executor: A=%d B=%d", execA.created, execB.created)
	}
}

// TestExecutorPoolFallback 无注册行 → 单点兜底（现有部署无感）。
func TestExecutorPoolFallback(t *testing.T) {
	pool := NewExecutorPool(&poolFakeExec{id: "fallback"}, nil)
	pool.ListExecutors = func(context.Context) ([]store.ExecutorRow, error) { return nil, nil }
	pool.SandboxOwner = func(context.Context, string) (string, error) { return "", nil }
	if _, err := pool.CreateSandbox(context.Background(), execproto.CreateSandboxRequest{SessionID: "s_f"}); err != nil {
		t.Fatalf("fallback create: %v", err)
	}
}

// TestSchedulerPolicyRoundRobinAndAffinity 策略缝矩阵。
func TestSchedulerPolicyRoundRobinAndAffinity(t *testing.T) {
	cands := []policy.ExecutorCandidate{{ID: "A"}, {ID: "B"}}
	// RoundRobin 确定性（同会话同选择）
	rr := policy.RoundRobin{}
	a := rr.Pick(context.Background(), "s_1", cands)
	b := rr.Pick(context.Background(), "s_1", cands)
	if a.ID != b.ID {
		t.Fatalf("RoundRobin 应确定性: %s vs %s", a.ID, b.ID)
	}
	// Affinity：归属命中优先
	aff := policy.AffinityBySession{Affinity: func(context.Context, string) string { return "B" }}
	if got := aff.Pick(context.Background(), "s_1", cands); got.ID != "B" {
		t.Fatalf("Affinity 应选归属: %s", got.ID)
	}
}
