package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
)

// mcpToolsFromState 把 session 的 MCP 连接展开为协议工具清单（名字 mcp:<server>:<tool>，
// schema 透传——真实模型凭 schema 填参数）。连接工具缓存为空时懒 tools/list。
func mcpToolsFromState(ctx context.Context, client MCPCaller, conns []MCPConnection) ([]runs.Tool, error) {
	if client == nil || len(conns) == 0 {
		return nil, nil
	}
	var out []runs.Tool
	for _, conn := range conns {
		specs := conn.Tools
		if len(specs) == 0 {
			var err error
			specs, err = client.ListTools(ctx, conn.URL)
			if err != nil {
				return nil, fmt.Errorf("mcp %s tools/list: %w", conn.Server, err)
			}
		}
		for _, t := range specs {
			out = append(out, runs.Tool{
				Type:      "function",
				Name:      runs.MCPToolPrefix + conn.Server + ":" + t.Name,
				RiskClass: 1, // MCP 工具默认 class 1（可经 tool_classes 覆盖升级）
				Schema:    t.Schema,
			})
		}
	}
	return out, nil
}

// dispatchMCP 调 MCP 工具（mcp:<server>:<tool> → 连接 URL + tools/call）。
// 结果 JSON 回喂；事件 mcp.call。
func dispatchMCP(ctx restate.Context, deps *Deps, in RunInput, runID string, step int, tc ToolCall, emit *Emitter) (string, float64, error) {
	if deps.MCP == nil {
		return "", 0, restate.ToTerminalError(fmt.Errorf("MCP 客户端未启用"))
	}
	server, tool, ok := strings.Cut(strings.TrimPrefix(tc.Name, runs.MCPToolPrefix), ":")
	if !ok || server == "" || tool == "" {
		return "", 0, restate.ToTerminalError(fmt.Errorf("MCP 工具名不规范（应为 mcp:<server>:<tool>）: %q", tc.Name))
	}
	state, err := deps.Sessions.GetState(ctx, in.SessionID)
	if err != nil {
		return "", 0, err
	}
	var url string
	for _, conn := range state.MCP {
		if conn.Server == server {
			url = conn.URL
			break
		}
	}
	if url == "" {
		return "", 0, restate.ToTerminalError(fmt.Errorf("MCP server %q 未连接", server))
	}
	result, callErr := deps.MCP.Call(ctx, url, tool, json.RawMessage(tc.Arguments))
	_ = emit.Emit(ctx, in.SessionID, runID, step, event.MCPCall, "mcp", tc.Name, map[string]any{
		"step": step, "server": server, "tool": tool,
		"arguments": json.RawMessage(tc.Arguments), "error": callErr != nil,
	})
	if callErr != nil {
		return fmt.Sprintf(`{"name":%q,"result":{"server":%q,"tool":%q,"error":%q}}`, tc.Name, server, tool, callErr.Error()), 0, nil
	}
	return fmt.Sprintf(`{"name":%q,"result":{"server":%q,"tool":%q,"content":%s}}`,
		tc.Name, server, tool, mustJSONString(result)), 0, nil
}
