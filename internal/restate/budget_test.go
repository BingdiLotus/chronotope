package restate

import (
	"context"
	"reflect"
	"testing"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/x/mocks"
	"github.com/stretchr/testify/mock"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

// TestOrgQuotaExceeded ② 级检查：token/计算秒双轴、0/缺省 = 无限、非法值忽略。
func TestOrgQuotaExceeded(t *testing.T) {
	st := &fakeStore{orgQuotas: map[string]any{store.QuotaDailyTokenBudget: float64(100)}}
	deps := &Deps{Store: st}

	st.orgTokens = 99
	if exceeded, _ := orgQuotaExceeded(t.Context(), deps, "org_test", time.Now()); exceeded {
		t.Fatal("99 < 100 不应超限")
	}
	st.orgTokens = 100
	if exceeded, key := orgQuotaExceeded(t.Context(), deps, "org_test", time.Now()); !exceeded || (key != store.QuotaDailyTokenBudget) {
		t.Fatalf("100 >= 100 应超限: %v %s", exceeded, key)
	}
	// 计算秒轴
	st.orgTokens = 0
	st.orgQuotas = map[string]any{store.QuotaDailyComputeBudget: float64(10)}
	st.orgCompute = 10
	if exceeded, key := orgQuotaExceeded(t.Context(), deps, "org_test", time.Now()); !exceeded || (key != store.QuotaDailyComputeBudget) {
		t.Fatalf("计算秒应超限: %v %s", exceeded, key)
	}
	// 缺省/非法 → 无限
	st.orgQuotas = map[string]any{store.QuotaDailyTokenBudget: "bad"}
	if exceeded, _ := orgQuotaExceeded(t.Context(), deps, "org_test", time.Now()); exceeded {
		t.Fatal("非法值应视为无限")
	}
	st.orgQuotas = nil
	if exceeded, _ := orgQuotaExceeded(t.Context(), deps, "org_test", time.Now()); exceeded {
		t.Fatal("无预算应视为无限")
	}
}

// newBudgetMockedLoop：Run 真执行 + Awakeable mock（freeze 挂起 → resolve "unfrozen"；
// resolve 时模拟充值：配额调大，后续每步检查不再超限）。
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

	out, err := runLoop(ctx, deps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
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

	out, err := runLoop(ctx, deps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
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
