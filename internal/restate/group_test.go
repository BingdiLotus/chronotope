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

// TestRunLoopGroupChat 群聊主持循环（落地方案 §14）：moderator 两次 next_speaker →
// 成员 child run 各发言一次（group.turn 归属）→ 结果回喂 → moderator 终答。
func TestRunLoopGroupChat(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	ha := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: runs.ToolNextSpeaker, Arguments: json.RawMessage(`{"participant":0,"instruction":"请发表意见"}`)}}},
		{Done: false, ToolCalls: []ToolCall{{ID: "t_2", Name: runs.ToolNextSpeaker, Arguments: json.RawMessage(`{"participant":1,"instruction":"请补充"}`)}}},
		{Done: true, Final: "讨论完成，达成共识。"},
	}}
	se := &fakeSessions{state: SessionState{
		Phase: sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{
			Model: "m", Instructions: "你是主持人。", Tools: []string{}, Version: 1,
		},
		Participants: []sessionapi.Participant{
			{AgentID: "a_member1", Role: "架构师"},
			{AgentID: "a_member2", Role: "评审"},
		},
	}}
	ex := &fakeExecutor{}

	// mock child workflow：按调用序返回成员发言
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
	speeches := []string{"我建议采用方案 A。", "我补充：A 的成本偏高。"}
	mockClient.EXPECT().Request(mock.Anything, mock.Anything).RunAndReturn(
		func(input, output any, _ ...restate.RequestOption) restate.TerminalError {
			i := len(st.createdSessions) - 1
			if i < 0 || i >= len(speeches) {
				i = len(speeches) - 1
			}
			reflect.ValueOf(output).Elem().Set(reflect.ValueOf(RunOutput{Final: speeches[i], Steps: 1}))
			return nil
		}).Maybe()

	ctx := restate.WithMockContext(mockCtx)
	out, err := runLoop(ctx, deps(st, ha, se, ex), RunInput{SessionID: "s_1", Input: "讨论方案"}, "r_1")
	if err != nil || out.Final != "讨论完成，达成共识。" || out.Steps != 3 {
		t.Fatalf("群聊 run 应完成: out=%+v err=%v", out, err)
	}
	// group.turn 事件 ×2（发言者归属）
	turns := eventsOf(st, event.GroupTurn)
	if len(turns) != 2 {
		t.Fatalf("应有 2 条 group.turn，得 %d", len(turns))
	}
	var tp struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal(turns[0].payload, &tp)
	if tp.AgentID != "a_member1" {
		t.Fatalf("首个发言者应为 a_member1: %s", turns[0].payload)
	}
	// 成员发言作为 tool 消息回喂（发言者归属经 child run 推导）
	var speechFound int
	for _, m := range st.messages {
		if m.role == "tool" && (strings.Contains(m.content, "方案 A") || strings.Contains(m.content, "成本偏高")) {
			speechFound++
		}
	}
	if speechFound != 2 {
		t.Fatalf("应回喂 2 条成员发言: %+v", st.messages)
	}
}

// TestGroupTools 群聊工具组装：participants 非空附加 next_speaker。
func TestGroupTools(t *testing.T) {
	cfg := sessionapi.AgentConfig{Model: "m", Instructions: "i", Tools: []string{}, Version: 1}
	plain := groupAwareTools(cfg, SessionState{})
	if len(plain) != 0 {
		t.Fatalf("普通会话不应有群聊工具: %+v", plain)
	}
	group := groupAwareTools(cfg, SessionState{Participants: []sessionapi.Participant{{AgentID: "a_1"}}})
	if len(group) != 1 || group[0].Name != runs.ToolNextSpeaker {
		t.Fatalf("群聊应附加 next_speaker: %+v", group)
	}
}
