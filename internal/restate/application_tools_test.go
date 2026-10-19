package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

type applicationStore struct {
	*fakeStore
	session  *store.Session
	terminal RunTerminalResult
	llm      map[int]*store.LLMCallRow
}

func (s *applicationStore) GetSession(context.Context, string) (*store.Session, error) {
	return s.session, nil
}
func (s *applicationStore) FinalizeRun(ctx context.Context, run, session string, status sessionapi.RunStatus, kind string, payload json.RawMessage) error {
	if err := json.Unmarshal(payload, &s.terminal); err != nil {
		return err
	}
	return s.fakeStore.FinalizeRun(ctx, run, session, status, kind, payload)
}
func (s *applicationStore) PutLLMCallPrepared(_ context.Context, _ string, step int, hash string) (*store.LLMCallRow, error) {
	if s.llm == nil {
		s.llm = map[int]*store.LLMCallRow{}
	}
	if row := s.llm[step]; row != nil {
		return row, nil
	}
	row := &store.LLMCallRow{State: "prepared", RequestHash: hash}
	s.llm[step] = row
	return row, nil
}
func (s *applicationStore) PutLLMCallResult(_ context.Context, _ string, step int, in, out int64, partial, unknown bool, errMsg, result, credential string) error {
	row := s.llm[step]
	row.State = "result"
	row.Result = result
	row.UsageUnknown = unknown
	if result == "" {
		row.State = "unknown"
	}
	return nil
}

func applicationFixture(t *testing.T) (*applicationStore, *fakeSessions, sessionapi.AgentConfig) {
	t.Helper()
	cfg := sessionapi.AgentConfig{Model: "fixture", Instructions: "application SOP", ApplicationProfile: true, ApplicationBridgeRef: "bots", MaxSteps: 4, MaxOutputTokens: 123, MaxDurationMS: 5000, Tools: []string{"bash"}, ApplicationTools: []sessionapi.ApplicationTool{
		{ModelAlias: "remember", ToolRef: "knowledge.remember", Description: "Remember scoped information", InputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}},"required":["content"],"additionalProperties":false}`)},
		{ModelAlias: "create_reminder", ToolRef: "task.create_reminder", Description: "Create one native reminder", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)},
	}}
	s := &applicationStore{fakeStore: &fakeStore{runs: map[string]*store.Run{"r_app": {ID: "r_app", SessionID: "s_app", Bound: map[string]any{"tenant_id": "tenant_one", "subject_ref": "bot_one", "scope_id": "channel_one", "origin_command_id": "command_one", "deadline_at": time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339Nano)}}}}, session: &store.Session{ID: "s_app", OrgID: "tenant_one", SubjectRef: "bot_one", ScopeID: "channel_one"}}
	return s, &fakeSessions{state: SessionState{Phase: sessionapi.PhaseReady, AgentConfig: cfg}}, cfg
}

func TestApplicationRunAllCallsAndRealResults(t *testing.T) {
	s, se, cfg := applicationFixture(t)
	nonce := fmt.Sprintf("sink-only-%d", time.Now().UnixNano())
	var calls []ApplicationToolCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("missing bridge auth")
		}
		var call ApplicationToolCall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		if call.Context.TenantID != "tenant_one" || call.Context.SubjectRef != "bot_one" || call.Context.ScopeID != "channel_one" || call.Context.OriginCommandID != "command_one" || call.Context.OperationID == "" || call.Context.ToolCallID == "" {
			t.Errorf("untrusted or missing context: %+v", call.Context)
		}
		if r.Header.Get("Idempotency-Key") != call.Context.OperationID {
			t.Error("operation header mismatch")
		}
		calls = append(calls, call)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"nonce": nonce, "tool": call.ToolRef})
	}))
	defer server.Close()
	client, err := NewApplicationToolClient(map[string]ApplicationBridgeConfig{"bots": {TenantID: "tenant_one", URL: server.URL, BearerToken: "private-token"}})
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHarness{script: []*Result{{Done: true, ToolCalls: []ToolCall{{ID: "call_1", Name: "remember", Arguments: json.RawMessage(`{"content":"meeting"}`)}, {ID: "call_2", Name: "create_reminder", Arguments: json.RawMessage(`{"text":"meeting"}`)}}}, {Done: true, Final: "stored"}}}
	d := &Deps{Store: s, Sessions: se, Harness: h, ApplicationTools: client, ConsolidateThreshold: 1}
	out, err := runLoop(newMockedLoop(t), d, RunInput{SessionID: "s_app", Input: "remember and remind"}, "r_app")
	if err != nil {
		t.Fatal(err)
	}
	if out.Final != "stored" || len(calls) != 2 || calls[0].ToolRef != "knowledge.remember" || calls[1].ToolRef != "task.create_reminder" {
		t.Fatalf("all tools not dispatched: out=%+v calls=%+v", out, calls)
	}
	if calls[0].Context.OperationID == calls[1].Context.OperationID {
		t.Fatal("tool operations collided")
	}
	request := h.calls[1]
	if request.MaxOutputTokens != 123 || request.TimeoutMS <= 0 || request.MaxTurns != 1 || len(request.Tools) != 2 || request.Tools[0].Description == "" || len(request.Tools[0].Schema) == 0 {
		t.Fatalf("model contract missing: %+v", request)
	}
	var outputs []runs.Message
	for _, m := range request.Messages {
		if m.Role == "tool" {
			outputs = append(outputs, m)
		}
	}
	if len(outputs) != 2 || outputs[0].ToolCallID != "call_1" || outputs[1].ToolCallID != "call_2" || !strings.Contains(outputs[0].Content, nonce) || !strings.Contains(outputs[1].Content, nonce) {
		t.Fatalf("real results not paired: %+v", outputs)
	}
	encoded, _ := json.Marshal(request)
	if strings.Contains(string(encoded), "private-token") || strings.Contains(string(encoded), server.URL) {
		t.Fatal("bridge secret/address leaked to model")
	}
	if s.terminal.Error != nil || s.terminal.Steps != 2 || !s.terminal.Usage.UsageUnknown || len(s.createdSummaries) > 0 {
		t.Fatalf("bad terminal or unexpected consolidation: %+v", s.terminal)
	}
	if s.llm[0].State != "result" || !s.llm[0].UsageUnknown {
		t.Fatal("known result lost when usage missing")
	}
	if cached, err := harnessCallClosure(fakeRunContext{context.Background()}, d, "r_app", 0, h.calls[0]); err != nil || cached == nil || len(cached.ToolCalls) != 2 || len(h.calls) != 2 {
		t.Fatalf("known result must replay without a new model request: cached=%+v err=%v", cached, err)
	}
	_ = cfg
}

type countingApplicationCaller struct {
	calls   int
	unknown bool
}

func (c *countingApplicationCaller) Call(context.Context, string, ApplicationToolCall) (json.RawMessage, error) {
	c.calls++
	if c.unknown {
		return nil, ErrApplicationResultUnknown
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func TestApplicationRunRejectedCallsArePairedWithoutDispatch(t *testing.T) {
	s, se, _ := applicationFixture(t)
	client := &countingApplicationCaller{}
	h := &fakeHarness{script: []*Result{{Done: true, ToolCalls: []ToolCall{
		{ID: "bad_json", Name: "remember", Arguments: json.RawMessage(`null`), RawArguments: `{"content":`, ArgumentsError: "invalid JSON"},
		{ID: "bad_schema", Name: "remember", Arguments: json.RawMessage(`{"content":123}`)},
		{ID: "not_allowed", Name: "bash", Arguments: json.RawMessage(`{"command":"echo forbidden"}`)},
	}}, {Done: true, Final: "corrected"}}}
	_, err := runLoop(newMockedLoop(t), &Deps{Store: s, Sessions: se, Harness: h, ApplicationTools: client}, RunInput{SessionID: "s_app", Input: "x"}, "r_app")
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 0 {
		t.Fatal("invalid/unregistered tool was dispatched")
	}
	var toolMessages int
	for _, m := range h.calls[1].Messages {
		if m.Role == "tool" {
			toolMessages++
			if m.ToolCallID == "" || !strings.Contains(m.Content, "error") {
				t.Fatalf("error not paired: %+v", m)
			}
		}
	}
	if toolMessages != 3 {
		t.Fatalf("got %d results", toolMessages)
	}
}

func TestApplicationUnknownStopsFollowingEffects(t *testing.T) {
	s, se, _ := applicationFixture(t)
	client := &countingApplicationCaller{unknown: true}
	h := &fakeHarness{script: []*Result{{Done: true, ToolCalls: []ToolCall{{ID: "one", Name: "remember", Arguments: json.RawMessage(`{"content":"x"}`)}, {ID: "two", Name: "create_reminder", Arguments: json.RawMessage(`{"text":"x"}`)}}}}}
	_, err := runLoop(newMockedLoop(t), &Deps{Store: s, Sessions: se, Harness: h, ApplicationTools: client}, RunInput{SessionID: "s_app", Input: "x"}, "r_app")
	if err == nil || client.calls != 1 || s.terminal.Error == nil {
		t.Fatalf("unknown was not fenced: %v calls=%d terminal=%+v", err, client.calls, s.terminal)
	}
	count := 0
	for _, m := range s.messages {
		if m.role == "tool" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("stopped calls must still be paired: %d", count)
	}
}

func TestApplicationExecutionLimitsStopBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"deadline", "steps", "truncated", "tenant", "scope"} {
		t.Run(mode, func(t *testing.T) {
			s, se, cfg := applicationFixture(t)
			client := &countingApplicationCaller{}
			h := &fakeHarness{script: []*Result{{Done: true, Truncated: mode == "truncated", ToolCalls: []ToolCall{{ID: "one", Name: "remember", Arguments: json.RawMessage(`{"content":"x"}`)}}}}}
			switch mode {
			case "deadline":
				s.runs["r_app"].Bound["deadline_at"] = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
			case "steps":
				cfg.MaxSteps = 1
				se.state.AgentConfig = cfg
			case "tenant":
				s.runs["r_app"].Bound["tenant_id"] = "other"
			case "scope":
				s.runs["r_app"].Bound["scope_id"] = "other"
			}
			_, err := runLoop(newMockedLoop(t), &Deps{Store: s, Sessions: se, Harness: h, ApplicationTools: client}, RunInput{SessionID: "s_app", Input: "x"}, "r_app")
			if err == nil || s.terminal.Error == nil {
				t.Fatalf("limit/binding was ignored: %v", err)
			}
			if mode != "steps" && client.calls != 0 {
				t.Fatal("blocked tool dispatched")
			}
			if (mode == "deadline" || mode == "tenant" || mode == "scope") && len(h.calls) != 0 {
				t.Fatal("blocked model dispatched")
			}
		})
	}
}

func TestApplicationBridgeTenantAndRedirectAreFenced(t *testing.T) {
	calls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{}`)) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, err := NewApplicationToolClient(map[string]ApplicationBridgeConfig{"bots": {TenantID: "one", URL: redirect.URL, BearerToken: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"other", "one"} {
		_, err := client.Call(context.Background(), "bots", ApplicationToolCall{Context: ApplicationCallContext{TenantID: tenant}})
		if err == nil {
			t.Fatal("tenant mismatch/redirect accepted")
		}
	}
	if calls != 0 {
		t.Fatal("redirect forwarded authenticated tool call")
	}
}
