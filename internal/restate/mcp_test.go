package restate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/x/mocks"
	"github.com/stretchr/testify/mock"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// TestHTTPMCPClient 客户端编解码：initialize/tools/list/tools/call + 错误。
func TestHTTPMCPClient(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		methods = append(methods, req.Method)
		switch req.Method {
		case "initialize":
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "tools/list":
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
				"tools": []map[string]any{{"name": "echo", "description": "回显", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}},
			}})
		case "tools/call":
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "echo: 你好"}},
			}})
		default:
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "unknown"}})
		}
	}))
	defer srv.Close()

	client := NewHTTPMCPClient()
	specs, err := client.ListTools(context.Background(), srv.URL)
	if err != nil || len(specs) != 1 || specs[0].Name != "echo" {
		t.Fatalf("ListTools: %+v err=%v", specs, err)
	}
	if specs[0].Schema == nil {
		t.Fatal("schema 应透传（真实模型凭 schema 填参数）")
	}
	out, err := client.Call(context.Background(), srv.URL, "echo", json.RawMessage(`{"text":"你好"}`))
	if err != nil || out != "echo: 你好" {
		t.Fatalf("Call: %q err=%v", out, err)
	}
	if methods[0] != "tools/list" || methods[1] != "tools/call" {
		t.Fatalf("方法序不符: %v", methods)
	}
	// 未知方法 → error 分支
	if _, err := client.Call(context.Background(), srv.URL+"x", "x", nil); err == nil {
		t.Fatal("连接失败应报错")
	}
}

// TestMCPToolsFromState 连接展开为协议工具清单（mcp:<server>:<tool> + schema）。
func TestMCPToolsFromState(t *testing.T) {
	client := &fakeMCP{specs: []MCPToolSpec{{Name: "echo", Schema: json.RawMessage(`{"type":"object"}`)}}}
	conns := []MCPConnection{{Server: "s1", URL: "http://mcp"}}
	tools, err := mcpToolsFromState(context.Background(), client, conns, nil)
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools: %+v err=%v", tools, err)
	}
	if tools[0].Name != "mcp:s1:echo" || tools[0].Schema == nil || tools[0].RiskClass != 1 {
		t.Fatalf("工具形状不符: %+v", tools[0])
	}
}

// TestRunLoopMCPCall 真实 dispatch：mcp:<server>:<tool> → 客户端调用 → 回喂 + 事件。
func TestRunLoopMCPCall(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	ha := &fakeHarness{script: []*Result{
		{Done: false, ToolCalls: []ToolCall{{ID: "t_m", Name: "mcp:echo:echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}},
		{Done: true, Final: "MCP 结果已获取。"},
	}}
	se := &fakeSessions{state: SessionState{
		Phase: sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{
			Model: "m", Instructions: "i", Tools: []string{}, Version: 1,
		},
		MCP: []MCPConnection{{Server: "echo", URL: "http://mcp.example"}},
	}}
	mcpClient := &fakeMCP{callResult: "echo: hi"}

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
	d := deps(st, ha, se, &fakeExecutor{})
	d.MCP = mcpClient
	out, err := runLoop(ctx, d, RunInput{SessionID: "s_1", Input: "调用 echo"}, "r_1")
	if err != nil || out.Final != "MCP 结果已获取。" {
		t.Fatalf("run: %+v err=%v", out, err)
	}
	// mcp.call 事件 + 结果回喂
	calls := eventsOf(st, event.MCPCall)
	if len(calls) != 1 || !strings.Contains(string(calls[0].payload), `"server":"echo"`) {
		t.Fatalf("mcp.call 事件不符: %+v", calls)
	}
	if !strings.Contains(st.messages[2].content, "echo: hi") {
		t.Fatalf("MCP 结果应回喂: %s", st.messages[2].content)
	}
	if mcpClient.calledWith != "echo" {
		t.Fatalf("应调 echo 工具: %v", mcpClient.calledWith)
	}
}

type fakeMCP struct {
	specs      []MCPToolSpec
	callResult string
	callErr    error
	calledWith string
}

func (f *fakeMCP) ListTools(context.Context, string) ([]MCPToolSpec, error) {
	return f.specs, nil
}
func (f *fakeMCP) Call(_ context.Context, _, tool string, _ json.RawMessage) (string, error) {
	f.calledWith = tool
	if f.callErr != nil {
		return "", f.callErr
	}
	return f.callResult, nil
}
