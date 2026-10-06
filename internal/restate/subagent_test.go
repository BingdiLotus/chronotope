package restate

import (
	"context"
	"encoding/json"
	"fmt"
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

// newSubagentMockedLoop：Run 真执行 + Workflow mock（child run 返回指定结果/错误）。
func newSubagentMockedLoop(t *testing.T, childFinal string, childErr error) (restate.Context, *fakeStore, *fakeSessions) {
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
	mockClient := mocks.NewMockClient(t)
	mockCtx.EXPECT().Workflow(RunWorkflowName, mock.Anything, "run").Return(mockClient).Maybe()
	mockClient.EXPECT().Request(mock.Anything, mock.Anything).RunAndReturn(
		func(input, output any, _ ...restate.RequestOption) restate.TerminalError {
			t.Logf("child 请求到达: childErr=%v", childErr)
			if childErr != nil {
				return restate.AsTerminalError(childErr)
			}
			reflect.ValueOf(output).Elem().Set(reflect.ValueOf(RunOutput{Final: childFinal, Steps: 2}))
			return nil
		}).Maybe()

	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	se := &fakeSessions{state: SessionState{
		Phase: sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{
			Model: "m", Instructions: "i", Tools: []string{"spawn_subagent"}, Version: 1,
		},
	}}
	return restate.WithMockContext(mockCtx), st, se
}

// TestRunLoopSpawnSubagent 子 Agent 成功路径：建子会话/子 run（确定性 id）→
// child 完成 → subagent.spawned/completed 事件 → 结果回喂 → 父 agent 继续终答。
func TestRunLoopSpawnSubagent(t *testing.T) {
	ctx, st, se := newSubagentMockedLoop(t, "子任务完成：42。", nil)
	ha := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_sub", Name: "spawn_subagent", Arguments: json.RawMessage(`{"agent":"a_child","input":"计算 2+2"}`)}}},
		{Done: true, Final: "父任务完成，引用子结果。"},
	}}
	ex := &fakeExecutor{}

	out, err := runLoop(ctx, deps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "派子任务"}, "r_1")
	if err != nil || out.Final != "父任务完成，引用子结果。" || out.Steps != 2 {
		t.Fatalf("父 run 应完成: out=%+v err=%v", out, err)
	}
	// 子会话/子 run 建行（确定性 id 经 journaled Run）
	if len(st.createdSessions) != 1 || !strings.HasPrefix(st.createdSessions[0], "s_sub_") {
		t.Fatalf("应建 1 个子会话（s_sub_ 前缀）: %+v", st.createdSessions)
	}
	spawned := eventsOf(st, event.SubagentSpawned)
	completed := eventsOf(st, event.SubagentCompleted)
	if len(spawned) != 1 || len(completed) != 1 {
		t.Fatalf("应有 spawned/completed 事件各 1 条: %d/%d", len(spawned), len(completed))
	}
	var sp struct {
		ChildRunID string `json:"child_run_id"`
		Input      string `json:"input"`
	}
	_ = json.Unmarshal(spawned[0].payload, &sp)
	if sp.ChildRunID == "" || sp.Input != "计算 2+2" {
		t.Fatalf("spawned 载荷不符: %s", spawned[0].payload)
	}
	// 结果回喂：tool 消息含子任务 final
	if len(st.messages) != 3 { // user + tool + assistant
		t.Fatalf("消息应为 3 条（user/tool/assistant），得 %d", len(st.messages))
	}
	if !strings.Contains(st.messages[1].content, "子任务完成：42。") {
		t.Fatalf("tool 结果应含子任务 final: %s", st.messages[1].content)
	}
}

// TestRunLoopSpawnSubagentChildFailure 子任务失败不致命：错误回喂，父 run 照常完成。
func TestRunLoopSpawnSubagentChildFailure(t *testing.T) {
	ctx, st, se := newSubagentMockedLoop(t, "", restate.ToTerminalError(fmt.Errorf("子任务预算超限")))
	ha := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_sub", Name: "spawn_subagent", Arguments: json.RawMessage(`{"agent":"a_child","input":"大任务"}`)}}},
		{Done: true, Final: "子任务失败，父继续。"},
	}}
	ex := &fakeExecutor{}

	out, err := runLoop(ctx, deps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "派子任务"}, "r_1")
	if err != nil || out.Final != "子任务失败，父继续。" {
		t.Fatalf("子任务失败不应 fail 父 run: out=%+v err=%v", out, err)
	}
	completed := eventsOf(st, event.SubagentCompleted)
	if len(completed) != 1 || !strings.Contains(string(completed[0].payload), "子任务预算超限") {
		t.Fatalf("completed 事件应携带错误: %+v", completed)
	}
	if !strings.Contains(st.messages[1].content, "子任务预算超限") {
		t.Fatalf("tool 结果应携带错误: %s", st.messages[1].content)
	}
}

var _ = runs.ToolSpawnSubagent
