package restate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
)

// mcpToolsFromState 把 session 的 MCP 连接展开为协议工具清单（名字 mcp:<server>:<tool>，
// schema 透传——真实模型凭 schema 填参数）。连接工具缓存为空时懒 tools/list。
func mcpToolsFromState(ctx context.Context, client MCPCaller, conns []MCPConnection, allowlist func(server, tool string) (bool, error)) ([]runs.Tool, error) {
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
			// MCP 网关 allowlist（期 3 §C 基础设施安全面）：未列工具不下发
			//（无行 = 全拒，默认安全）；allowlist nil = 未启用网关（旧行为）
			if allowlist != nil {
				allowed, aErr := allowlist(conn.Server, t.Name)
				if aErr != nil {
					return nil, fmt.Errorf("mcp allowlist: %w", aErr)
				}
				if !allowed {
					continue
				}
			}
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
	// 执行层 allowlist 防御（清单层过滤之外的兜底——fake/异常调用绕过清单
	// 仍会到执行层；w13 断言稳定失败实证：allowlist 拒绝的工具仍发 mcp.call）
	if sess, sErr := deps.Store.GetSession(ctx, in.SessionID); sErr == nil {
		// 只拦「显式配置且不匹配」（无配置 = 旧 MCP 路径放行——W6 历史段
		// 兼容；有配置则按 allowlist 拒发——w13 语义）
		if has, hErr := deps.Store.HasMCPAllowlist(ctx, sess.OrgID, server); hErr == nil && has {
			if allowed, aErr := deps.Store.MCPToolAllowed(ctx, sess.OrgID, server, tool); aErr == nil && !allowed {
				return "", 0, restate.ToTerminalError(fmt.Errorf("MCP 工具 %s 不在 org allowlist（过滤拒发）", tc.Name))
			}
		}
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
	// 审计 #10：MCP 调用 journaled（restate.Run 缓存结果——重放不重调外部
	// 工具；「效果已发生、确认未存」的 receipt ledger 窗口后置标注）
	result, callErr := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
		// 审计 4.1 仲裁：账本失败零派发；unknown 停派发（SDK 重试不得覆盖）
		callKey := tc.ID
		if callKey == "" {
			callKey = fmt.Sprintf("%x", sha256.Sum256(tc.Arguments))[:16]
		}
		reqHash := fmt.Sprintf("%x", sha256.Sum256([]byte(server+"|"+tool+"|"+string(tc.Arguments))))[:32]
		row, err := deps.Store.PutMCPCallPrepared(rc, runID, step, server, tool, callKey, reqHash)
		if err != nil {
			return "", restate.ToTerminalError(fmt.Errorf("mcp 账本 prepare 失败（零派发）: %w", err))
		}
		if row == nil {
			return "", restate.ToTerminalError(fmt.Errorf("mcp 账本 prepare 无行（零派发）"))
		}
		if row.State == "result" {
			// 审计 4.1（九期）：result 分支回读冻结原结果（hash 同 → 原值；
			// hash 异 → operation 冲突拒绝——不再普通 error 进 SDK retryable）
			if row.RequestHash != "" && row.RequestHash != reqHash {
				return "", restate.ToTerminalError(fmt.Errorf("mcp 账本结果与请求 hash 不符（operation 冲突）"))
			}
			if row.Result != "" {
				return row.Result, nil // 冻结原结果——零派发
			}
			return "", restate.ToTerminalError(fmt.Errorf("mcp 账本已有结果（无法回读——人工裁决）"))
		}
		if row.State == "unknown" {
			return "", restate.ToTerminalError(fmt.Errorf("mcp 账本 unknown 停派发（需人工裁决）"))
		}
		r, err := deps.MCP.Call(rc, url, tool, json.RawMessage(tc.Arguments))
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		resultJSON := ""
		if err == nil {
			resultJSON = r
		}
		if lErr := deps.Store.PutMCPCallResult(rc, runID, step, server, tool, callKey, reqHash, resultJSON, errMsg); lErr != nil {
			return "", restate.ToTerminalError(fmt.Errorf("mcp 账本 result 失败: %w", lErr))
		}
		return r, err
	}, restate.WithName(StepName("mcp", step, tc.Name)))
	_ = emit.Emit(ctx, in.SessionID, runID, step, event.MCPCall, "mcp", tc.Name, map[string]any{
		"step": step, "server": server, "tool": tool,
		"arguments": json.RawMessage(tc.Arguments), "error": callErr != nil,
	})
	if callErr != nil {
		// 仲裁类错误（账本 unknown 停派发）→ terminal（人工裁决）；
		// 其余按工具错误结果回喂模型
		if strings.Contains(callErr.Error(), "停派发") || strings.Contains(callErr.Error(), "账本") {
			return "", 0, restate.ToTerminalError(callErr)
		}
		return fmt.Sprintf(`{"name":%q,"result":{"server":%q,"tool":%q,"error":%q}}`, tc.Name, server, tool, callErr.Error()), 0, nil
	}
	return fmt.Sprintf(`{"name":%q,"result":{"server":%q,"tool":%q,"content":%s}}`,
		tc.Name, server, tool, mustJSONString(result)), 0, nil
}
