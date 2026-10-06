package restate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

// TestRunLoopNoProgress 边界语义 §1：连续 5 轮状态指纹不变 → run.failed{no_progress}。
func TestRunLoopNoProgress(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	ha := &fakeHarness{script: func() []*Result {
		var s []*Result
		for i := 0; i < 6; i++ { // 6 次相同 bash（第 5 次重复即熔断，共 dispatch 4 次）
			s = append(s, &Result{Done: false, ToolCalls: []ToolCall{{ID: "t_bash", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}}})
		}
		return s
	}()}
	se := &fakeSessions{state: SessionState{
		Phase: sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{
			Model: "m", Instructions: "i", Tools: []string{"bash"}, Version: 1,
		},
	}}
	ex := &fakeExecutor{}

	_, err := runLoop(newMockedLoop(t), deps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("应因无进展终止，得 %v", err)
	}
	failed := eventsOf(st, event.RunFailed)
	if len(failed) != 1 {
		t.Fatalf("应有 1 条 run.failed，得 %d", len(failed))
	}
	var fp struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(failed[0].payload, &fp); err != nil || fp.Reason != "no_progress" {
		t.Fatalf("run.failed reason 应为 no_progress: %s err=%v", failed[0].payload, err)
	}
	// 第 5 次重复在 dispatch 前熔断 → bash 只执行 4 次
	if len(ex.execs) != 4 {
		t.Fatalf("熔断前应 dispatch 4 次，得 %d", len(ex.execs))
	}
	if r, ok := st.runs["r_1"]; !ok || r.Status != sessionapi.RunFailed {
		t.Fatalf("run 状态应为 failed: %+v", st.runs)
	}
}

// TestRunLoopBudgetExceeded 边界语义 §1：run 级预算（token 超限 → budget.exceeded + failed）。
func TestRunLoopBudgetExceeded(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	ha := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: "bash", Arguments: json.RawMessage(`{"command":"a"}`)}}, Usage: llmUsage(10, 10)},
		{Done: false, ToolCalls: []ToolCall{{ID: "t_2", Name: "bash", Arguments: json.RawMessage(`{"command":"b"}`)}}, Usage: llmUsage(10, 10)},
	}}
	se := &fakeSessions{state: SessionState{
		Phase: sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{
			Model: "m", Instructions: "i", Tools: []string{"bash"}, Version: 1,
			Budget: map[string]any{"max_tokens": float64(30)}, // 累计 40 > 30 → 第 2 步熔断
		},
	}}
	ex := &fakeExecutor{}

	_, err := runLoop(newMockedLoop(t), deps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("应因预算超限终止，得 %v", err)
	}
	if got := eventsOf(st, event.BudgetExceeded); len(got) != 1 {
		t.Fatalf("应有 1 条 budget.exceeded 事件，得 %d", len(got))
	}
	failed := eventsOf(st, event.RunFailed)
	var fp struct {
		Reason string `json:"reason"`
	}
	if len(failed) != 1 {
		t.Fatalf("应有 1 条 run.failed，得 %d", len(failed))
	}
	if err := json.Unmarshal(failed[0].payload, &fp); err != nil || fp.Reason != "budget_exceeded" {
		t.Fatalf("run.failed reason 应为 budget_exceeded: %s err=%v", failed[0].payload, err)
	}
	if r, ok := st.runs["r_1"]; !ok || r.Status != sessionapi.RunFailed {
		t.Fatalf("run 状态应为 failed: %+v", st.runs)
	}
}

// TestRunLoopBudgetUnderLimit 预算未超限 → 正常完成（熔断不误伤）。
func TestRunLoopBudgetUnderLimit(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	ha := &fakeHarness{script: []*Result{{
		Done: true, Final: "完成。",
		Usage: llmUsage(5, 5),
	}}}
	se := &fakeSessions{state: SessionState{
		Phase: sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{
			Model: "m", Instructions: "i", Tools: []string{}, Version: 1,
			Budget: map[string]any{"max_tokens": float64(100)},
		},
	}}
	ex := &fakeExecutor{}
	out, err := runLoop(newMockedLoop(t), deps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
	if err != nil || out.Final != "完成。" {
		t.Fatalf("未超限应正常完成，得 out=%+v err=%v", out, err)
	}
	if got := eventsOf(st, event.BudgetExceeded); len(got) != 0 {
		t.Fatalf("不应有 budget.exceeded: %d", len(got))
	}
}

func llmUsage(in, out int) runs.LLMUsage {
	return runs.LLMUsage{TokensIn: in, TokensOut: out}
}
