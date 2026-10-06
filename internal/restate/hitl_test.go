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

// TestRunLoopRequestApprovalHITL：控制工具 → awakeable 挂起 → awaiting_approval 事件
// → resolve 后 resumed 事件 → 结果回喂 → 下一轮终答。
func TestRunLoopRequestApprovalHITL(t *testing.T) {
	store := &fakeStore{}
	harness := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: "request_approval", Arguments: json.RawMessage(`{"question":"允许吗？"}`)}}},
		{Done: true, Final: "已获批准，继续。", Usage: runs.LLMUsage{TokensIn: 2, TokensOut: 1}},
	}}
	sessions := &fakeSessions{state: SessionState{
		Phase:       sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1},
	}}
	exec := &fakeExecutor{}

	mockCtx := mocks.NewMockContext(t)
	// Run 真执行闭包（await 的 Run 闭包会调 awakeable.Result → mock 返回 "approve"）
	mockCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(f func(restate.RunContext) (any, error), output any, _ ...restate.RunOption) restate.TerminalError {
			v, err := f(fakeRunContext{context.Background()})
			if err != nil {
				return restate.AsTerminalError(err)
			}
			reflect.ValueOf(output).Elem().Set(reflect.ValueOf(v))
			return nil
		}).Maybe()
	// Awakeable：id 记录 + Result 返回审批结果
	mockA := mocks.NewMockAwakeableFuture(t)
	mockCtx.EXPECT().Awakeable().Return(mockA).Maybe()
	mockA.EXPECT().Id().Return("aw_1").Maybe()
	mockA.EXPECT().Result(mock.Anything).RunAndReturn(func(output any) restate.TerminalError {
		reflect.ValueOf(output).Elem().Set(reflect.ValueOf("approve"))
		return nil
	}).Maybe()

	ctx := restate.WithMockContext(mockCtx)
	out, err := runLoop(ctx, deps(store, harness, sessions, exec), RunInput{SessionID: "s_1", Input: "go"}, "r_1")
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if out.Final != "已获批准，继续。" || out.Steps != 2 {
		t.Fatalf("输出不符: %+v", out)
	}

	// 挂起前记录 awakeable id（webhook 据此 resolve）
	if sessions.pending != "aw_1" {
		t.Fatalf("awakeable id 应存入会话状态: %q", sessions.pending)
	}
	// 事件：awaiting_approval → resumed（挂起期间零进程占用由 Restate 保证）
	awaiting := eventsOf(store, event.RunAwaitingApproval)
	if len(awaiting) != 1 {
		t.Fatalf("应有 1 条 run.awaiting_approval，得 %d", len(awaiting))
	}
	var ap struct {
		AwakeableID string `json:"awakeable_id"`
	}
	_ = json.Unmarshal(awaiting[0].payload, &ap)
	if ap.AwakeableID != "aw_1" {
		t.Fatalf("事件应携带 awakeable_id: %+v", ap)
	}
	if got := eventsOf(store, event.RunResumed); len(got) != 1 {
		t.Fatalf("应有 1 条 run.resumed，得 %d", len(got))
	}
	// 审批结果回喂：tool 消息携带 approved 值
	second := harness.calls[1]
	last := second.Messages[len(second.Messages)-1]
	if !strings.Contains(last.Content, "approve") {
		t.Fatalf("tool 消息应携带审批结果: %s", last.Content)
	}
}

// TestWebhookResolveApproval：run_id → session → PendingAwakeable → resolve（幂等路径）。
func TestWebhookResolveApproval(t *testing.T) {
	store := &fakeStore{runs: map[string]*store.Run{
		"r_1": {ID: "r_1", SessionID: "s_1"},
	}}
	sessions := &fakeSessions{state: SessionState{PendingAwakeable: "aw_9"}}
	deps := &Deps{Store: store, Sessions: sessions}

	mockCtx := mocks.NewMockContext(t)
	mockCtx.EXPECT().ResolveAwakeable("aw_9", "approve").Once()
	ctx := restate.WithMockContext(mockCtx)

	got, err := resolveApproval(ctx, deps, WebhookResolveInput{RunID: "r_1", Payload: "approve"})
	if err != nil || got != "resolved" {
		t.Fatalf("resolve 失败: %v %q", err, got)
	}
	mockCtx.AssertExpectations(t)
}

func TestWebhookResolveNoPending(t *testing.T) {
	store := &fakeStore{runs: map[string]*store.Run{
		"r_1": {ID: "r_1", SessionID: "s_1"},
	}}
	sessions := &fakeSessions{state: SessionState{}}
	deps := &Deps{Store: store, Sessions: sessions}

	mockCtx := mocks.NewMockContext(t)
	ctx := restate.WithMockContext(mockCtx)

	if _, err := resolveApproval(ctx, deps, WebhookResolveInput{RunID: "r_1", Payload: "x"}); err == nil {
		t.Fatal("无挂起审批应报错")
	}
}

// TestResolveApprovalDigestBinding 评审 #5：审批精确绑定动作摘要与审批者。
func TestResolveApprovalDigestBinding(t *testing.T) {
	// digest 不匹配 → 拒绝（run 保持挂起，不 resolve）
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	sessions := &fakeSessions{state: SessionState{
		PendingAwakeable: "aw_9", PendingActionDigest: "aaaa1111",
	}}
	mockCtx := mocks.NewMockContext(t) // 无 ResolveAwakeable 期望：若被调用即失败
	ctx := restate.WithMockContext(mockCtx)
	_, err := resolveApproval(ctx, &Deps{Store: st, Sessions: sessions}, WebhookResolveInput{
		RunID: "r_1", Payload: "已批准", ActionDigest: "bbbb2222", Approver: "ops@x",
	})
	if err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("digest 不匹配应拒绝: %v", err)
	}
	mockCtx.AssertExpectations(t)

	// digest 匹配 + approver → resolve + audit.approval 事件（审批者留痕）
	st2 := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	sessions2 := &fakeSessions{state: SessionState{
		PendingAwakeable: "aw_9", PendingActionDigest: "aaaa1111",
	}}
	mockCtx2 := mocks.NewMockContext(t)
	mockCtx2.EXPECT().ResolveAwakeable("aw_9", "已批准").Once()
	got, err := resolveApproval(ctx2(mockCtx2), &Deps{Store: st2, Sessions: sessions2}, WebhookResolveInput{
		RunID: "r_1", Payload: "已批准", ActionDigest: "aaaa1111", Approver: "ops@x",
	})
	if err != nil || got != "resolved" {
		t.Fatalf("digest 匹配应 resolve: %v %q", err, got)
	}
	audit := eventsOf(st2, event.AuditApproval)
	if len(audit) != 1 || !strings.Contains(string(audit[0].payload), "ops@x") {
		t.Fatalf("audit.approval 应记录审批者: %+v", audit)
	}
	mockCtx2.AssertExpectations(t)

	// 旧客户端（无 digest）→ legacy 放行（后向兼容）+ 审计标记
	st3 := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	sessions3 := &fakeSessions{state: SessionState{PendingAwakeable: "aw_9", PendingActionDigest: "aaaa1111"}}
	mockCtx3 := mocks.NewMockContext(t)
	mockCtx3.EXPECT().ResolveAwakeable("aw_9", "approve").Once()
	got, err = resolveApproval(ctx2(mockCtx3), &Deps{Store: st3, Sessions: sessions3}, WebhookResolveInput{
		RunID: "r_1", Payload: "approve",
	})
	if err != nil || got != "resolved" {
		t.Fatalf("legacy 应放行: %v %q", err, got)
	}
	if audit := eventsOf(st3, event.AuditApproval); len(audit) != 1 || !strings.Contains(string(audit[0].payload), "\"legacy\":true") {
		t.Fatalf("legacy 审计应标记: %+v", audit)
	}
	mockCtx3.AssertExpectations(t)
}

// ctx2 从 mock context 构造 restate 上下文（测试内联）。
func ctx2(mockCtx *mocks.MockContext) restate.Context {
	return restate.WithMockContext(mockCtx)
}
