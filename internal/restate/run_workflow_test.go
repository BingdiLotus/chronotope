package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/x/mocks"
	"github.com/stretchr/testify/mock"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/execproto"
	"github.com/bingdilotus/chronotope/internal/store"
)

// --- fakes（消费者侧接口的测试替身）---

type storedEvent struct {
	sessionID string
	runID     string
	typ       event.Type
	payload   json.RawMessage
	dedupe    string
}

type fakeStore struct {
	events   []storedEvent
	messages []struct {
		sessionID, runID, role, content string
		step                            int
	}
}

func (f *fakeStore) AppendEvent(_ context.Context, sessionID, runID string, typ event.Type, payload json.RawMessage, dedupeKey string) (int64, error) {
	f.events = append(f.events, storedEvent{sessionID, runID, typ, payload, dedupeKey})
	return int64(len(f.events)), nil
}

func (f *fakeStore) AppendMessage(_ context.Context, sessionID, runID string, step int, role string, content json.RawMessage) error {
	f.messages = append(f.messages, struct {
		sessionID, runID, role, content string
		step                            int
	}{sessionID, runID, role, string(content), step})
	return nil
}

func (f *fakeStore) ListMessages(context.Context, string, int) ([]store.Message, error) {
	return nil, nil
}

// fakeHarness 按脚本顺序返回结果，并记录每次请求（断言消息组装）。
type fakeHarness struct {
	script []*Result
	calls  []*runs.Request
}

func (f *fakeHarness) Call(_ context.Context, req *runs.Request) (*Result, error) {
	if len(f.calls) >= len(f.script) {
		return nil, fmt.Errorf("fakeHarness: 超出脚本（共 %d 次调用）", len(f.script))
	}
	r := f.script[len(f.calls)]
	f.calls = append(f.calls, req)
	return r, nil
}

type fakeSessions struct {
	state    SessionState
	attached []string
}

func (f *fakeSessions) GetState(_ restate.Context, _ string) (SessionState, error) {
	return f.state, nil
}

func (f *fakeSessions) AttachSandbox(_ restate.Context, _, sandboxID string) error {
	f.attached = append(f.attached, sandboxID)
	return nil
}

// fakeExecutor 实现 Executor 接口（W2 工具分流的测试替身）。
type fakeExecutor struct {
	execs   []string // "name:input"
	files   map[string]string
	created int
}

func (f *fakeExecutor) CreateSandbox(context.Context, execproto.CreateSandboxRequest) (string, error) {
	f.created++
	return "sb_test", nil
}

func (f *fakeExecutor) Execute(_ context.Context, sandboxID, name, input, idempotencyKey string) (*ExecResult, error) {
	f.execs = append(f.execs, name+":"+input)
	return &ExecResult{Exit: 0, Output: "ok\n"}, nil
}

func (f *fakeExecutor) ReadFile(_ context.Context, sandboxID, path string) (string, error) {
	return f.files[path], nil
}

func (f *fakeExecutor) WriteFile(_ context.Context, sandboxID, path, content string) error {
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = content
	return nil
}

// fakeRunContext 实现 restate.RunContext（Run 闭包的入参）。
type fakeRunContext struct{ context.Context }

func (fakeRunContext) Log() *slog.Logger         { return slog.Default() }
func (fakeRunContext) Request() *restate.Request { return nil }

// newMockedLoop 组装 mocks.MockContext：Run 真实执行闭包（fake harness 在闭包内被调用），
// 并把闭包结果反射写回 output 指针——与 x/mocks 的 RunAndReturn 同构。
func newMockedLoop(t *testing.T) restate.Context {
	t.Helper()
	mockCtx := mocks.NewMockContext(t)
	// Run(fn, output, opts...)：restate.Run 带 WithName 选项 → 3 个实参。
	// Maybe()：早期返回路径（会话未初始化等）不会触发 Run，命中次数由 fakeHarness 断言。
	mockCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(f func(restate.RunContext) (any, error), output any, _ ...restate.RunOption) restate.TerminalError {
			v, err := f(fakeRunContext{context.Background()})
			if err != nil {
				return restate.AsTerminalError(err)
			}
			reflect.ValueOf(output).Elem().Set(reflect.ValueOf(v))
			return nil
		}).Maybe()
	return restate.WithMockContext(mockCtx)
}

func deps(store *fakeStore, harness *fakeHarness, sessions *fakeSessions, executor *fakeExecutor) *Deps {
	return &Deps{Store: store, Harness: harness, Executor: executor, Sessions: sessions}
}

func eventsOf(store *fakeStore, typ event.Type) []storedEvent {
	var out []storedEvent
	for _, e := range store.events {
		if e.typ == typ {
			out = append(out, e)
		}
	}
	return out
}

// --- 用例 ---

func TestRunLoopHappyPath(t *testing.T) {
	store := &fakeStore{}
	harness := &fakeHarness{script: []*Result{{
		Done: true, Deltas: []string{"你好，"}, Final: "你好，我是助手。",
		Usage: runs.LLMUsage{TokensIn: 10, TokensOut: 5},
	}}}
	sessions := &fakeSessions{state: SessionState{
		Phase: sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{
			Model: "claude-sonnet-4-6", Instructions: "你是助手。", Version: 1,
		},
	}}
	exec := &fakeExecutor{}
	ctx := newMockedLoop(t)

	out, err := runLoop(ctx, deps(store, harness, sessions, exec), RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if out.Final != "你好，我是助手。" || out.Steps != 1 {
		t.Fatalf("输出不符: %+v", out)
	}

	// 事件序列：run.started → llm.call → run.completed
	if got := eventsOf(store, event.RunStarted); len(got) != 1 {
		t.Fatalf("应有 1 条 run.started，得 %d", len(got))
	}
	if got := eventsOf(store, event.LLMCall); len(got) != 1 {
		t.Fatalf("应有 1 条 llm.call，得 %d", len(got))
	}
	completed := eventsOf(store, event.RunCompleted)
	if len(completed) != 1 {
		t.Fatalf("应有 1 条 run.completed，得 %d", len(completed))
	}
	var cp struct {
		Final     string `json:"final"`
		Truncated bool   `json:"truncated"`
		V         int    `json:"v"`
	}
	if err := json.Unmarshal(completed[0].payload, &cp); err != nil {
		t.Fatalf("payload 解析: %v", err)
	}
	if cp.Final != "你好，我是助手。" || cp.V != 1 {
		t.Fatalf("run.completed payload 不符: %+v", cp)
	}

	// 消息写回：assistant 终答必须落表（重放后历史不缺失，worker-架构设计 §3）
	if len(store.messages) != 1 || store.messages[0].role != "assistant" {
		t.Fatalf("assistant 消息应写回: %+v", store.messages)
	}
	var msgText string
	if err := json.Unmarshal([]byte(store.messages[0].content), &msgText); err != nil {
		t.Fatalf("消息 content 应为 JSON 字符串: %v", err)
	}
	if msgText != "你好，我是助手。" {
		t.Fatalf("消息内容不符: %q", msgText)
	}

	// /runs 请求组装：system 指令 + 用户输入 + 模型绑定
	if len(harness.calls) != 1 {
		t.Fatalf("harness 应被调用 1 次，得 %d", len(harness.calls))
	}
	req := harness.calls[0]
	if req.Model != "claude-sonnet-4-6" || req.RunID != "r_1" || req.Step != 0 {
		t.Fatalf("请求字段不符: %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Content != "hi" {
		t.Fatalf("消息组装不符: %+v", req.Messages)
	}
}

func TestRunLoopToolCallContinuesLoop(t *testing.T) {
	store := &fakeStore{}
	harness := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}}},
		{Done: true, Final: "完成。", Usage: runs.LLMUsage{TokensIn: 5, TokensOut: 2}},
	}}
	sessions := &fakeSessions{state: SessionState{
		Phase:       sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1},
	}}
	exec := &fakeExecutor{}
	ctx := newMockedLoop(t)

	out, err := runLoop(ctx, deps(store, harness, sessions, exec), RunInput{SessionID: "s_1", Input: "go"}, "r_1")
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if out.Steps != 2 {
		t.Fatalf("工具交棒后应继续循环共 2 步，得 %d", out.Steps)
	}
	if len(harness.calls) != 2 {
		t.Fatalf("harness 应被调用 2 次，得 %d", len(harness.calls))
	}
	// 代码工具 → executor：沙箱懒创建 + 执行
	if exec.created != 1 {
		t.Fatalf("沙箱应懒创建 1 次，得 %d", exec.created)
	}
	if len(exec.execs) != 1 || exec.execs[0] != "bash:ls" {
		t.Fatalf("executor 应执行 bash:ls，得 %v", exec.execs)
	}
	if len(sessions.attached) != 1 || sessions.attached[0] != "sb_test" {
		t.Fatalf("沙箱 id 应回填会话状态: %v", sessions.attached)
	}
	// 第二步的请求应携带上一步的 tool_result（作为 tool 消息回喂，位于消息尾部）
	second := harness.calls[1]
	if n := len(second.Messages); n < 3 {
		t.Fatalf("第二步应携带 tool 消息: %+v", second.Messages)
	}
	if last := second.Messages[len(second.Messages)-1]; last.Role != "tool" || last.Source != "sandbox" {
		t.Fatalf("tool 消息应在尾部且标 source=sandbox（注入防护）: %+v", last)
	}
	if !strings.Contains(second.Messages[len(second.Messages)-1].Content, `"exit":0`) {
		t.Fatalf("tool 消息应携带 exit 结果: %+v", second.Messages[len(second.Messages)-1])
	}
	// 事件：tool.call / sandbox.exec / step.journaled 落表
	if got := eventsOf(store, event.ToolCall); len(got) != 1 {
		t.Fatalf("应有 1 条 tool.call，得 %d", len(got))
	}
	if got := eventsOf(store, event.SandboxExec); len(got) != 1 {
		t.Fatalf("应有 1 条 sandbox.exec，得 %d", len(got))
	}
	if got := eventsOf(store, event.StepJournaled); len(got) != 1 {
		t.Fatalf("应有 1 条 step.journaled，得 %d", len(got))
	}
}

func TestRunLoopHarnessErrorTerminates(t *testing.T) {
	store := &fakeStore{}
	harness := &fakeHarness{script: []*Result{{ErrCode: "max_turns", ErrMsg: "exceeded"}}}
	sessions := &fakeSessions{state: SessionState{
		Phase:       sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1},
	}}
	exec := &fakeExecutor{}
	ctx := newMockedLoop(t)

	_, err := runLoop(ctx, deps(store, harness, sessions, exec), RunInput{SessionID: "s_1", Input: "x"}, "r_1")
	if err == nil {
		t.Fatal("harness error 终态应返回错误")
	}
	if got := eventsOf(store, event.RunFailed); len(got) != 1 {
		t.Fatalf("应有 1 条 run.failed，得 %d", len(got))
	}
	var fp struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(eventsOf(store, event.RunFailed)[0].payload, &fp)
	if fp.Code != "max_turns" {
		t.Fatalf("run.failed 应带错误码: %+v", fp)
	}
}

func TestRunLoopUninitializedSession(t *testing.T) {
	store := &fakeStore{}
	harness := &fakeHarness{}
	sessions := &fakeSessions{state: SessionState{}}
	exec := &fakeExecutor{}
	ctx := newMockedLoop(t)

	_, err := runLoop(ctx, deps(store, harness, sessions, exec), RunInput{SessionID: "s_1", Input: "x"}, "r_1")
	if err == nil {
		t.Fatal("未初始化会话应报错")
	}
	if got := eventsOf(store, event.RunStarted); len(got) != 0 {
		t.Fatal("会话无效时不应发射 run.started")
	}
}

func TestRunLoopMaxSteps(t *testing.T) {
	store := &fakeStore{}
	// 永不 done 的脚本：每次返回工具调用，打到 maxSteps 封顶
	script := make([]*Result, 0, maxSteps)
	for i := 0; i < maxSteps; i++ {
		script = append(script, &Result{ToolCalls: []ToolCall{{ID: "t", Name: "bash"}}})
	}
	harness := &fakeHarness{script: script}
	sessions := &fakeSessions{state: SessionState{
		Phase:       sessionapi.PhaseReady,
		SandboxID:   "sb_test", // 预设沙箱：短路懒创建（该用例聚焦 maxSteps）
		AgentConfig: sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1},
	}}
	exec := &fakeExecutor{}
	ctx := newMockedLoop(t)

	_, err := runLoop(ctx, deps(store, harness, sessions, exec), RunInput{SessionID: "s_1", Input: "x"}, "r_1")
	if err == nil {
		t.Fatal("maxSteps 应报错")
	}
	if len(harness.calls) != maxSteps {
		t.Fatalf("harness 应被调用 %d 次，得 %d", maxSteps, len(harness.calls))
	}
	failed := eventsOf(store, event.RunFailed)
	if len(failed) != 1 {
		t.Fatalf("应有 1 条 run.failed，得 %d", len(failed))
	}
	var fp struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(failed[0].payload, &fp)
	if fp.Reason != "max_steps" {
		t.Fatalf("失败原因应为 max_steps: %+v", fp)
	}
}
