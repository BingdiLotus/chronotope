package restate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// MCP 工具分发（落地方案 §11）：worker 托管 MCP 客户端，harness 仍只见 schema、
// 零状态。HTTP transport（JSON-RPC 2.0）；stdio 型（沙箱内跑进程）后置。
// 工具名规范 mcp:<server>:<tool>（runs.MCPToolPrefix 通配，契约规范 §3）。

// MCPCaller 是 MCP 客户端接口（单测注入替身）。
type MCPCaller interface {
	ListTools(ctx context.Context, url string) ([]MCPToolSpec, error)
	Call(ctx context.Context, url, tool string, arguments json.RawMessage) (string, error)
}

// MCPToolSpec 是 tools/list 的工具描述（schema 透传真实模型，缺 schema 模型
// 返回空参数——real-group e2e 实证）。
type MCPToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

// HTTPMCPClient 是 HTTP transport 的 JSON-RPC 2.0 客户端。
type HTTPMCPClient struct {
	HTTP *http.Client
}

func NewHTTPMCPClient() *HTTPMCPClient {
	return &HTTPMCPClient{HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type mcpRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *HTTPMCPClient) rpc(ctx context.Context, url, method string, params json.RawMessage) (json.RawMessage, error) {
	body, _ := json.Marshal(mcpRPCRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", method, err)
	}
	defer resp.Body.Close()
	var out mcpRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("mcp: %s: decode: %w", method, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("mcp: %s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

// ListTools 调 tools/list，解析 [{name, description, inputSchema}]。
func (c *HTTPMCPClient) ListTools(ctx context.Context, url string) ([]MCPToolSpec, error) {
	result, err := c.rpc(ctx, url, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil, fmt.Errorf("mcp: tools/list: parse: %w", err)
	}
	out := make([]MCPToolSpec, 0, len(payload.Tools))
	for _, t := range payload.Tools {
		out = append(out, MCPToolSpec{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	return out, nil
}

// Call 调 tools/call，返回 text content 拼接（isError 视为错误）。
func (c *HTTPMCPClient) Call(ctx context.Context, url, tool string, arguments json.RawMessage) (string, error) {
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": arguments})
	result, err := c.rpc(ctx, url, "tools/call", params)
	if err != nil {
		return "", err
	}
	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", fmt.Errorf("mcp: tools/call: parse: %w", err)
	}
	var out string
	for i, c := range payload.Content {
		if c.Type == "text" {
			if i > 0 {
				out += "\n"
			}
			out += c.Text
		}
	}
	if payload.IsError {
		return out, fmt.Errorf("mcp: tools/call: %s", out)
	}
	return out, nil
}
