package restate

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
	"github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/x/mocks"
	"github.com/stretchr/testify/mock"
)

// TestJournalCacheReplay 修正方案「真实故障注入回归」之三：既有 journal
// 的缓存重放（golden_test 是「全新 journal 跑两次」验证确定性；这里验证
// 「既有 journal 命中」的语义）：
//
//	第一次：Run 真实执行闭包（harness/executor 被调用）→ 记录每次 Run 的返回；
//	第二次：Run 按序返回记录值（不执行闭包）→ 断言
//	  (a) 终态与第一次一致（重放确定性）
//	  (b) harness 零调用（既有 journal 命中 → 账本 result 分支零派发）
//	  (c) executor 零调用（exec 的 journal 命中 → 不重复外部效果）
//
// 这是「效果成功但结果未确认」类崩溃恢复的最小可验证形式（真实 kill-9
// 演练在 e2e 层另行推进）。
func TestJournalCacheReplay(t *testing.T) {
	script := []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: "write_file", Arguments: json.RawMessage(`{"path":"/workspace/r.py","content":"print(7)"}`)}}},
		{Done: true, Final: "重放闭环完成。"},
	}
	newScenario := func() (*fakeStore, *fakeHarness, *fakeSessions, *fakeExecutor) {
		st := &fakeStore{runs: map[string]*store.Run{"r_rp": {ID: "r_rp", SessionID: "s_rp"}}}
		ha := &fakeHarness{script: script}
		se := &fakeSessions{state: SessionState{
			Phase: sessionapi.PhaseReady,
			AgentConfig: sessionapi.AgentConfig{
				Model: "claude-sonnet-4-6", Instructions: "你是助手。",
				Tools: []string{"write_file"}, Version: 1,
			},
		}}
		ex := &fakeExecutor{files: map[string]string{}}
		return st, ha, se, ex
	}
	// cachedRunLoop：Run 按序返回 recorded（不执行闭包——既有 journal 命中）
	cachedRunLoop := func(t *testing.T, recorded []any) restate.Context {
		t.Helper()
		mockCtx := mocks.NewMockContext(t)
		mockCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
			func(_ func(restate.RunContext) (any, error), output any, _ ...restate.RunOption) restate.TerminalError {
				var v any
				if len(recorded) > 0 {
					v, recorded = recorded[0], recorded[1:]
				}
				reflect.ValueOf(output).Elem().Set(reflect.ValueOf(v))
				return nil
			}).Maybe()
		return restate.WithMockContext(mockCtx)
	}

	// —— 第一次：真实执行，记录 Run 的返回值 ——
	var recorded []any
	st1, ha1, se1, ex1 := newScenario()
	mockCtx := mocks.NewMockContext(t)
	mockCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(f func(restate.RunContext) (any, error), output any, _ ...restate.RunOption) restate.TerminalError {
			v, err := f(fakeRunContext{context.Background()})
			if err != nil {
				return restate.AsTerminalError(err)
			}
			recorded = append(recorded, v)
			reflect.ValueOf(output).Elem().Set(reflect.ValueOf(v))
			return nil
		}).Maybe()
	out1, err := runLoop(restate.WithMockContext(mockCtx), deps(st1, ha1, se1, ex1),
		RunInput{SessionID: "s_rp", Input: "写文件"}, "r_rp")
	if err != nil {
		t.Fatalf("第一次 runLoop: %v", err)
	}
	if len(ha1.calls) == 0 {
		t.Fatal("第一次 harness 应被调用（前置：真实执行）")
	}
	if len(recorded) == 0 {
		t.Fatal("第一次应记录到 journal 条目")
	}

	// —— 第二次：既有 journal 重放（Run 全部缓存命中）——
	st2, ha2, se2, ex2 := newScenario()
	out2, err := runLoop(cachedRunLoop(t, recorded), deps(st2, ha2, se2, ex2),
		RunInput{SessionID: "s_rp", Input: "写文件"}, "r_rp")
	if err != nil {
		t.Fatalf("重放 runLoop: %v", err)
	}
	// (a) 终态一致
	if out1.Final != out2.Final || out1.Steps != out2.Steps {
		t.Fatalf("重放终态不一致: first=%+v second=%+v", out1, out2)
	}
	// (b) harness 零调用（既有 journal 命中——账本 result 分支零派发）
	if len(ha2.calls) != 0 {
		t.Fatalf("重放时 harness 被调用 %d 次（既有 journal 应零派发）", len(ha2.calls))
	}
	// (c) executor 零调用（exec 的 journal 命中——不重复外部效果）
	if len(ex2.ops) != 0 {
		t.Fatalf("重放时 executor 被调用 %d 次（journal 命中不应重复外部效果）", len(ex2.ops))
	}
	// (d) 事件不重复增长（第一次 vs 重放）
	if len(st2.events) > len(st1.events) {
		t.Fatalf("重放产生了新事件（%d → %d）", len(st1.events), len(st2.events))
	}
	t.Logf("既有 journal 重放：harness=0 executor=0 events %d→%d final=%q",
		len(st1.events), len(st2.events), out2.Final)
	_ = strings.TrimSpace
}
