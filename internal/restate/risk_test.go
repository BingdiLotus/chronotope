package restate

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/x/mocks"
	"github.com/stretchr/testify/mock"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

func TestRiskClassOf(t *testing.T) {
	cfg := sessionapi.AgentConfig{ToolClasses: map[string]int{"bash": 2, "write_file": 1}}
	cases := []struct {
		name string
		want int
	}{
		{"read_file", 0},    // 内置 class 0
		{"list_files", 0},   // 内置 class 0
		{"bash", 2},         // 配置覆盖（收紧到 class 2）
		{"write_file", 1},   // 配置显式 1
		{"run_python", 1},   // 内置 class 1
		{"unknown_tool", 1}, // 未知保守 class 1
	}
	for _, c := range cases {
		if got := riskClassOf(c.name, cfg); got != c.want {
			t.Fatalf("riskClassOf(%s) = %d, want %d", c.name, got, c.want)
		}
	}
	// 非法值忽略（0/负数/超界按内置默认）
	bad := sessionapi.AgentConfig{ToolClasses: map[string]int{"bash": 0, "read_file": 9}}
	if got := riskClassOf("bash", bad); got != 1 {
		t.Fatalf("非法覆盖 0 应忽略（回默认 1），得 %d", got)
	}
	if got := riskClassOf("read_file", bad); got != 0 {
		t.Fatalf("非法覆盖 9 应忽略（回默认 0），得 %d", got)
	}
}

func TestApprovalGranted(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`{"approved":true}`, true},
		{`{"approved":false,"note":"拒绝"}`, false},
		{"已批准", true}, // 纯文本（W3 HITL 兼容）视为批准
		{`{}`, true},  // 缺字段视为批准
	}
	for _, c := range cases {
		if got := approvalGranted(c.in); got != c.want {
			t.Fatalf("approvalGranted(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestToolsFromConfigRiskClasses(t *testing.T) {
	cfg := sessionapi.AgentConfig{
		Tools:       []string{"read_file", "bash"},
		ToolClasses: map[string]int{"bash": 2},
	}
	tools := toolsFromConfig(cfg)
	if len(tools) != 2 {
		t.Fatalf("应 2 个工具，得 %d", len(tools))
	}
	byName := map[string]runs.Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	if byName["read_file"].RiskClass != 0 || byName["bash"].RiskClass != 2 {
		t.Fatalf("风险分级不符: %+v", tools)
	}
}

// newHITLMockedLoop 与 hitl_test 同构：Run 真执行 + Awakeable mock（approve/deny）。
func newHITLMockedLoop(t *testing.T, decision string) (restate.Context, *fakeSessions) {
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
	mockA.EXPECT().Id().Return("aw_risk").Maybe()
	mockA.EXPECT().Result(mock.Anything).RunAndReturn(func(output any) restate.TerminalError {
		reflect.ValueOf(output).Elem().Set(reflect.ValueOf(decision))
		return nil
	}).Maybe()
	return restate.WithMockContext(mockCtx), &fakeSessions{state: SessionState{
		Phase:       sessionapi.PhaseReady,
		SandboxID:   "sb_test",
		AgentConfig: sessionapi.AgentConfig{Model: "m", Instructions: "i", Tools: []string{"bash"}, ToolClasses: map[string]int{"bash": 2}, Version: 1},
	}}
}

// TestRunLoopClass2ForcedApproval 边界语义 §2：class 2 工具强制审批——批准后照常执行。
func TestRunLoopClass2ForcedApproval(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	ha := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}}},
		{Done: true, Final: "执行完成。"},
	}}
	ex := &fakeExecutor{}
	ctx, sessions := newHITLMockedLoop(t, "已批准")

	out, err := runLoop(ctx, deps(st, ha, sessions, ex), RunInput{SessionID: "s_1", Input: "go"}, "r_1")
	if err != nil || out.Final != "执行完成。" {
		t.Fatalf("批准后应正常执行: out=%+v err=%v", out, err)
	}
	awaiting := eventsOf(st, event.RunAwaitingApproval)
	if len(awaiting) != 1 {
		t.Fatalf("应有 1 条 run.awaiting_approval，得 %d", len(awaiting))
	}
	var ap struct {
		RiskClass int `json:"risk_class"`
	}
	_ = json.Unmarshal(awaiting[0].payload, &ap)
	if ap.RiskClass != 2 {
		t.Fatalf("审批事件应携带 risk_class=2: %s", awaiting[0].payload)
	}
	if len(ex.execs) != 1 {
		t.Fatalf("批准后 bash 应执行 1 次，得 %d", len(ex.execs))
	}
	if got := eventsOf(st, event.AuditToolDenied); len(got) != 0 {
		t.Fatalf("不应有 tool_denied")
	}
}

// TestRunLoopClass2Denied 拒绝 → audit.tool_denied + run.failed{tool_denied}，工具不执行。
func TestRunLoopClass2Denied(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	ha := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf /"}`)}}},
		{Done: true, Final: "不应到达。"},
	}}
	ex := &fakeExecutor{}
	ctx, sessions := newHITLMockedLoop(t, `{"approved":false,"note":"危险操作拒绝"}`)

	if got := riskClassOf("bash", sessions.state.AgentConfig); got != 2 {
		t.Fatalf("前置：bash 应为 class 2，得 %d", got)
	}
	t.Logf("approvalGranted 直测: %v", approvalGranted(`{"approved":false,"note":"危险操作拒绝"}`))
	_, err := runLoop(ctx, deps(st, ha, sessions, ex), RunInput{SessionID: "s_1", Input: "go"}, "r_1")
	t.Logf("awaiting=%d denied=%d failed=%d execs=%d err=%v",
		len(eventsOf(st, event.RunAwaitingApproval)), len(eventsOf(st, event.AuditToolDenied)),
		len(eventsOf(st, event.RunFailed)), len(ex.execs), err)
	if r := eventsOf(st, event.RunResumed); len(r) > 0 {
		t.Logf("resumed payload: %s", r[0].payload)
	}
	if err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("应因审批拒绝终止，得 %v", err)
	}
	if len(ex.execs) != 0 {
		t.Fatalf("拒绝后 bash 不得执行（永不自动执行），得 %d", len(ex.execs))
	}
	if got := eventsOf(st, event.AuditToolDenied); len(got) != 1 {
		t.Fatalf("应有 1 条 audit.tool_denied，得 %d", len(got))
	}
	failed := eventsOf(st, event.RunFailed)
	if len(failed) != 1 {
		t.Fatalf("应有 1 条 run.failed，得 %d", len(failed))
	}
	var fp struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(failed[0].payload, &fp)
	if fp.Reason != "tool_denied" {
		t.Fatalf("run.failed reason 应为 tool_denied: %s", failed[0].payload)
	}
	if r, ok := st.runs["r_1"]; !ok || r.Status != sessionapi.RunFailed {
		t.Fatalf("run 状态应为 failed: %+v", st.runs)
	}
}
