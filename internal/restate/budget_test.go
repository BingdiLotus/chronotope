package restate

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/x/mocks"
	"github.com/stretchr/testify/mock"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/policy"
	"github.com/bingdilotus/chronotope/internal/store"
)

// TestOrgBudgetSnapshotAndCheck ② 级快照判定（正确性二期 ⑧）：
// token/计算秒双轴、0/缺省 = 无限、非法值忽略、快照+累计语义。
func TestOrgBudgetSnapshotAndCheck(t *testing.T) {
	st := &fakeStore{orgQuotas: map[string]any{store.QuotaDailyTokenBudget: float64(100)}}
	p := policy.OrgDailyBudget{
		Org: func(_ context.Context, _ string) (map[string]any, error) { return st.orgQuotas, nil },
		Usage: func(_ context.Context, _ string, _ time.Time) (int64, float64, error) {
			return st.orgTokens, st.orgCompute, nil
		},
	}

	st.orgTokens = 99
	snap := p.SnapshotOrg(t.Context(), "org_test")
	if snap.QuotaTokens != 100 || snap.UsageTokens != 99 {
		t.Fatalf("快照不符: %+v", snap)
	}
	if d := p.Check(t.Context(), snap, policy.Accum{}); d.Allow == false {
		t.Fatal("99 < 100 不应超限")
	}
	// 本 run 累计使快照超限（其他会话用量不变）
	if d := p.Check(t.Context(), snap, policy.Accum{Tokens: 1}); d.Allow || d.Key != store.QuotaDailyTokenBudget {
		t.Fatalf("99+1 >= 100 应超限: %+v", d)
	}
	// 计算秒轴
	st.orgTokens = 0
	st.orgQuotas = map[string]any{store.QuotaDailyComputeBudget: float64(10)}
	st.orgCompute = 10
	snap = p.SnapshotOrg(t.Context(), "org_test")
	if d := p.Check(t.Context(), snap, policy.Accum{}); d.Allow || d.Key != store.QuotaDailyComputeBudget {
		t.Fatalf("计算秒应超限: %+v", d)
	}
	// 缺省/非法 → 无限
	st.orgQuotas = map[string]any{store.QuotaDailyTokenBudget: "bad"}
	snap = p.SnapshotOrg(t.Context(), "org_test")
	if snap.QuotaTokens != 0 {
		t.Fatalf("非法值应视为无限: %+v", snap)
	}
	st.orgQuotas = nil
	snap = p.SnapshotOrg(t.Context(), "org_test")
	if d := p.Check(t.Context(), snap, policy.Accum{Tokens: 100000}); !d.Allow {
		t.Fatal("无预算应视为无限")
	}
}

// TestRunLoopBudgetSnapshotDeterministic ⑧ 确定性：run 执行中其他会话推进 org
// 用量 → 本 run 的每步检查仍用入口快照（分支不变）。
func TestRunLoopBudgetSnapshotDeterministic(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	ha := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo hi"}`)}}},
		{Done: true, Final: "完成。"},
	}}
	se := &fakeSessions{state: SessionState{
		Phase: sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{
			Model: "m", Instructions: "i", Tools: []string{"bash"}, Version: 1,
		},
	}}
	ex := &fakeExecutor{}
	st.orgQuotas = map[string]any{store.QuotaDailyTokenBudget: float64(100)}
	st.orgTokens = 90 // 入口快照 90 < 100

	mockCtx := mocks.NewMockContext(t)
	mockCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(f func(restate.RunContext) (any, error), output any, _ ...restate.RunOption) restate.TerminalError {
			v, err := f(fakeRunContext{context.Background()})
			if err != nil {
				return restate.AsTerminalError(err)
			}
			reflect.ValueOf(output).Elem().Set(reflect.ValueOf(v))
			return nil
		}).Maybe()

	ctx := restate.WithMockContext(mockCtx)
	// 首次 harness 调用前把 org 用量推进到 200（模拟其他会话消费）——
	// 旧实现每步读库会冻结；新实现用入口快照（90）不受影响
	ha.onCall = func() { st.orgTokens = 200 }
	out, err := runLoop(ctx, budgetDeps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "x"}, "r_1")
	if err != nil || out.Final != "完成。" {
		t.Fatalf("快照确定性：应正常完成: %+v err=%v", out, err)
	}
	frozen := eventsOf(st, event.RunFrozen)
	if len(frozen) != 0 {
		t.Fatalf("执行中推进的用量不得触发本 run 冻结（快照确定性）: %+v", frozen)
	}
}

// newBudgetMockedLoop：Run 真执行 + Awakeable mock（freeze 挂起 → resolve "unfrozen"；
// resolve 时模拟充值：配额调大，后续每步检查不再超限）。
// budgetDeps 为预算测试装配参考实现（期 3 §A：策略缝注入——w5 语义平移）。
func budgetDeps(st *fakeStore, ha *fakeHarness, se *fakeSessions, ex *fakeExecutor) *Deps {
	d := deps(st, ha, se, ex)
	d.BudgetPolicy = &policy.OrgDailyBudget{
		Org: func(_ context.Context, _ string) (map[string]any, error) { return st.orgQuotas, nil },
		Usage: func(_ context.Context, _ string, _ time.Time) (int64, float64, error) {
			return st.orgTokens, st.orgCompute, nil
		},
	}
	return d
}

func newBudgetMockedLoop(t *testing.T, st *fakeStore) (restate.Context, *fakeSessions) {
	t.Helper()
	mockCtx := mocks.NewMockContext(t)
	mockCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(f func(restate.RunContext) (any, error), output any, _ ...restate.RunOption) restate.TerminalError {
			v, err := f(fakeRunContext{context.Background()})
			if err != nil {
				return restate.AsTerminalError(err)
			}
			reflect.ValueOf(output).Elem().Set(reflect.ValueOf(v))
			return nil
		}).Maybe()
	mockA := mocks.NewMockAwakeableFuture(t)
	mockCtx.EXPECT().Awakeable().Return(mockA).Maybe()
	mockA.EXPECT().Id().Return("aw_freeze").Maybe()
	mockA.EXPECT().Result(mock.Anything).RunAndReturn(func(output any) restate.TerminalError {
		st.orgQuotas = map[string]any{store.QuotaDailyTokenBudget: float64(1e9)} // 模拟充值
		reflect.ValueOf(output).Elem().Set(reflect.ValueOf("unfrozen"))
		return nil
	}).Maybe()
	se := &fakeSessions{state: SessionState{
		Phase:       sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1},
	}}
	return restate.WithMockContext(mockCtx), se
}

// TestRunLoopOrgBudgetFreezeAndUnfreeze：入口超限 → run.frozen + budget.exceeded →
// Unfreeze resolve（mock 立即返回 unfrozen）→ run.unfrozen → 正常完成。
func TestRunLoopOrgBudgetFreezeAndUnfreeze(t *testing.T) {
	st := &fakeStore{
		runs:      map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}},
		orgQuotas: map[string]any{store.QuotaDailyTokenBudget: float64(50)},
		orgTokens: 60, // 已超限
	}
	ha := &fakeHarness{script: []*Result{{Done: true, Final: "完成。"}}}
	ex := &fakeExecutor{}
	ctx, se := newBudgetMockedLoop(t, st)

	out, err := runLoop(ctx, budgetDeps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
	if err != nil || out.Final != "完成。" {
		t.Fatalf("解冻后应正常完成: out=%+v err=%v", out, err)
	}
	// 冻结事件链：budget.exceeded → run.frozen → run.unfrozen
	if got := eventsOf(st, event.BudgetExceeded); len(got) != 1 {
		t.Fatalf("应有 1 条 budget.exceeded，得 %d", len(got))
	}
	if got := eventsOf(st, event.RunFrozen); len(got) != 1 {
		t.Fatalf("应有 1 条 run.frozen，得 %d", len(got))
	}
	if got := eventsOf(st, event.RunUnfrozen); len(got) != 1 {
		t.Fatalf("应有 1 条 run.unfrozen，得 %d", len(got))
	}
	// 冻结槽已记录（Unfreeze 的依据）
	if se.frozen != "aw_freeze" {
		t.Fatalf("冻结 awakeable 应入槽: %q", se.frozen)
	}
}

// TestRunLoopOrgBudgetNotExceeded 未超限不冻结（无 frozen 事件，正常完成）。
func TestRunLoopOrgBudgetNotExceeded(t *testing.T) {
	st := &fakeStore{
		runs:      map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}},
		orgQuotas: map[string]any{store.QuotaDailyTokenBudget: float64(1e9)},
		orgTokens: 10,
	}
	ha := &fakeHarness{script: []*Result{{Done: true, Final: "完成。"}}}
	ex := &fakeExecutor{}
	ctx, se := newBudgetMockedLoop(t, st)

	out, err := runLoop(ctx, budgetDeps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
	if err != nil || out.Final != "完成。" {
		t.Fatalf("应正常完成: out=%+v err=%v", out, err)
	}
	if got := eventsOf(st, event.RunFrozen); len(got) != 0 {
		t.Fatalf("未超限不应冻结: %d", len(got))
	}
	if se.frozen != "" {
		t.Fatalf("不应入冻结槽: %q", se.frozen)
	}
}
