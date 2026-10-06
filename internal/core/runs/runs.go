// Package runs 是 /runs 协议的 Go 类型（契约规范 §3：worker ↔ harness 唯一协议）。
//
// 纪律：harness 无状态（每次调用全部上下文来自入参，输出只经 SSE 帧返回）；
// 幂等由 worker 侧 journal 缓存承担，harness 不实现缓存；`done` 是唯一合法终态。
package runs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProtocolVersion 是当前实现的协议版本；平台同时支持最近 2 个版本，run 绑定版本路由。
const ProtocolVersion = "1.0"

// Request 是 POST /runs 的请求体。
type Request struct {
	Protocol       string    `json:"protocol"` // 默认 "1.0"
	RunID          string    `json:"run_id"`   // 幂等键 (run_id, step)
	SessionID      string    `json:"session_id"`
	Step           int       `json:"step"`
	Model          string    `json:"model"`
	Messages       []Message `json:"messages"`
	Tools          []Tool    `json:"tools"`
	MaxTurns       int       `json:"max_turns"`        // 默认 8
	MaxOutputBytes int       `json:"max_output_bytes"` // 默认 512KB
}

// Message 是会话消息；source 标记用于注入防护（边界语义 §2）。
type Message struct {
	Role      string            `json:"role"` // system|user|assistant|tool
	Content   string            `json:"content"`
	ToolCalls []json.RawMessage `json:"tool_calls,omitempty"`
	Source    string            `json:"source,omitempty"` // trusted|sandbox|network
	// ToolCallID 关联上一条 assistant 工具调用（tool 消息回喂必需——
	// Anthropic 兼容 API 硬校验 'tool_call_id'，真实模型 e2e 实证缺失即 400）
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// Tool 是模型可见工具；risk_class 见边界语义 §2（0 安全 / 1 敏感 / 2 强制审批）。
type Tool struct {
	Type      string          `json:"type"`             // function|mcp
	Name      string          `json:"name"`             // 必须是工具名词汇表规范名
	Schema    json.RawMessage `json:"schema,omitempty"` // 空时省略，不得序列化为 null（契约样例）
	RiskClass int             `json:"risk_class"`
}

// FrameType 是 SSE 帧类型（契约规范 §3）。
type FrameType string

const (
	FrameDelta    FrameType = "delta"     // {text}
	FrameToolCall FrameType = "tool_call" // {id, name, arguments}——交棒，name 为词汇表规范名
	FrameTurnEnd  FrameType = "turn_end"  // {summary}
	FrameDone     FrameType = "done"      // {final, usage, truncated?}——唯一合法终态
	FrameError    FrameType = "error"     // {code, message}——失败终态
	FrameBeat     FrameType = "beat"      // {} 心跳帧（每 ≥30s 一发）
)

// Frame 是 /runs 的 SSE 帧（{type, seq, payload}）。
type Frame struct {
	Type    FrameType       `json:"type"`
	Seq     int             `json:"seq"`
	Payload json.RawMessage `json:"payload"`
}

// DonePayload 是 done 帧载荷；截断/断流时 usage 允许不完整（usage_partial）。
type DonePayload struct {
	Final        string   `json:"final"`
	Usage        LLMUsage `json:"usage"`
	Truncated    *bool    `json:"truncated,omitempty"` // 输出超 max_output_bytes 时为 true
	UsagePartial *bool    `json:"usage_partial,omitempty"`
}

// LLMUsage 与事件侧 llm.call.usage 同构。
type LLMUsage struct {
	TokensIn     int  `json:"tokens_in"`
	TokensOut    int  `json:"tokens_out"`
	UsagePartial bool `json:"usage_partial,omitempty"`
}

// ErrorPayload 是 error 帧载荷；code 语义见契约规范 §6。
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 工具名词汇表（契约规范 §3 硬约束，只增不改）：任何 harness 实现必须发出规范名。
const (
	ToolBash            = "bash"
	ToolRunPython       = "run_python"
	ToolReadFile        = "read_file"
	ToolWriteFile       = "write_file"
	ToolListFiles       = "list_files"
	ToolWebSearch       = "web_search"
	ToolHTTPRequest     = "http_request"
	ToolRequestApproval = "request_approval"
	ToolSpawnSubagent   = "spawn_subagent"
	ToolNextSpeaker     = "next_speaker" // 群聊：moderator 指定下一位发言者（W7）

	// MCPToolPrefix 是 mcp:<server>:<tool> 前缀。
	MCPToolPrefix = "mcp:"
)

// IsVocabularyName 校验工具名是否属于词汇表（含 mcp: 前缀通配）。
func IsVocabularyName(name string) bool {
	if strings.HasPrefix(name, MCPToolPrefix) {
		return true
	}
	switch name {
	case ToolBash, ToolRunPython, ToolReadFile, ToolWriteFile, ToolListFiles,
		ToolWebSearch, ToolHTTPRequest, ToolRequestApproval, ToolSpawnSubagent,
		ToolNextSpeaker:
		return true
	}
	return false
}

// Validate 做请求侧最小校验（骨架版：协议版本 + 幂等键必填）。
func (r Request) Validate() error {
	if r.Protocol != ProtocolVersion {
		return fmt.Errorf("protocol %q unsupported (want %s)", r.Protocol, ProtocolVersion)
	}
	if r.RunID == "" || r.SessionID == "" {
		return fmt.Errorf("run_id and session_id are required")
	}
	return nil
}
