package restate

import (
	"encoding/json"
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/execproto"
)

// execOutcome 是 journaled 的 exec 结果（含执行时长；重放回放同一时长——确定性）。
// 字段必须导出：SDK 以 JSON 序列化 journal 条目，未导出字段重放后为零值（nil 解引用实证）。
type execOutcome struct {
	Result   *ExecResult   `json:"result"`
	Duration time.Duration `json:"duration_ns"`
}

// dispatchTool 是四类工具路由（worker-架构设计 §1 dispatcher）的 W2 实现：
// 代码/命令类 → executor（本文件）；控制类（request_approval）→ W3 awakeable；
// MCP 类 → W6；API 类已在 harness 内联，不会到达这里。
// 返回 tool_result 内容（作为 tool 消息回喂模型）。
// dispatchTool 分流一个工具调用；返回 (结果 JSON 字符串, 计算秒, error)。
// 计算秒（沙箱执行时长）在 journaled Run 闭包内测量并随结果一起记账——
// 重放时回放同一时长（确定性），run 级预算熔断据此累加（边界语义 §1）。
func dispatchTool(ctx restate.Context, deps *Deps, in RunInput, runID string, step int, cfg sessionapi.AgentConfig, tc ToolCall, emit *Emitter) (string, float64, error) {
	switch tc.Name {
	case runs.ToolBash, runs.ToolRunPython, runs.ToolListFiles:
		input := codeToolInput(tc)
		sandboxID, err := ensureSandbox(ctx, deps, in.SessionID, cfg)
		if err != nil {
			return "", 0, err
		}
		// 时长在 Run 闭包内测量并随结果 journal（重放回放同一时长）
		outcome, err := restate.Run(ctx, func(rc restate.RunContext) (*execOutcome, error) {
			started := time.Now()
			res, err := deps.Executor.Execute(rc, sandboxID, tc.Name, input,
				execproto.ExecuteIdempotencyKey(runID, step, tc.ID))
			if err != nil {
				return nil, err
			}
			return &execOutcome{Result: res, Duration: time.Since(started)}, nil
		}, restate.WithName(StepName("exec", step, tc.ID)))
		if err != nil {
			return "", 0, err
		}
		res := outcome.Result
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.SandboxExec, "sandbox", tc.Name, map[string]any{
			"step": step, "tool": tc.Name, "exit": res.Exit,
			"output": truncate(res.Output, 4096), "output_ref": res.OutputRef, "truncated": res.Truncated,
			"duration_ms": outcome.Duration.Milliseconds(), // 计算秒计量依据（W4）+ 预算熔断
		})
		return jsonToolResult(tc.Name, res), outcome.Duration.Seconds(), nil

	case runs.ToolWriteFile:
		path, content := fileToolArgs(tc)
		if path == "" {
			return "", 0, restate.ToTerminalError(fmt.Errorf("write_file 缺 path"))
		}
		sandboxID, err := ensureSandbox(ctx, deps, in.SessionID, cfg)
		if err != nil {
			return "", 0, err
		}
		_, err = restate.Run(ctx, func(rc restate.RunContext) (string, error) {
			return "", deps.Executor.WriteFile(rc, sandboxID, path, content)
		}, restate.WithName(StepName("exec", step, tc.ID)))
		if err != nil {
			return "", 0, err
		}
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.SandboxExec, "sandbox", tc.Name, map[string]any{
			"step": step, "tool": tc.Name, "exit": 0, "path": path,
		})
		return fmt.Sprintf(`{"name":%q,"result":{"written":%q}}`, tc.Name, path), 0, nil

	case runs.ToolReadFile:
		path, _ := fileToolArgs(tc)
		if path == "" {
			return "", 0, restate.ToTerminalError(fmt.Errorf("read_file 缺 path"))
		}
		sandboxID, err := ensureSandbox(ctx, deps, in.SessionID, cfg)
		if err != nil {
			return "", 0, err
		}
		content, err := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
			return deps.Executor.ReadFile(rc, sandboxID, path)
		}, restate.WithName(StepName("exec", step, tc.ID)))
		if err != nil {
			return "", 0, err
		}
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.SandboxExec, "sandbox", tc.Name, map[string]any{
			"step": step, "tool": tc.Name, "exit": 0, "path": path,
		})
		return fmt.Sprintf(`{"name":%q,"result":{"content":%s}}`, tc.Name, mustJSONString(truncate(content, 4096))), 0, nil

	case runs.ToolSpawnSubagent:
		// 子 Agent：建子会话 + child run_workflow（durable 等待）+ 结果回喂（W6）
		return dispatchSubagent(ctx, deps, in, runID, step, cfg, tc, emit)

	case runs.ToolRequestApproval:
		// 控制类工具：awakeable 挂起（零进程占用），webhook resolve 后继续（W3 HITL）
		decision, err := awaitApproval(ctx, deps, in, runID, step, tc, emit, 0)
		if err != nil {
			return "", 0, err
		}
		return fmt.Sprintf(`{"name":%q,"result":{"approved":%s}}`, tc.Name, mustJSONString(decision)), 0, nil

	default:
		return "", 0, restate.ToTerminalError(fmt.Errorf("工具 %q 不支持（四类路由：代码→executor / 控制→awakeable / MCP→W6 / API→harness 内联）", tc.Name))
	}
}

// awaitApproval 是 HITL 控制工具的挂起/恢复路径（worker-架构设计 §3 的 ControlTool 分支）：
// 建 awakeable（journaled）→ id 存入会话状态 → awaiting_approval 事件 →
// 直接阻塞在 Result()（挂起 = 零进程占用；SDK 1.x 禁在 Run 闭包内使用 Context 操作，
// 阻塞本身即挂起点）→ webhook resolve 后从 journal 恢复 → resumed 事件 → 结果回喂。
// awaitApproval 挂起审批（HITL；riskClass 0 = 控制工具自身的审批请求，
// 2 = class 2 危险工具的强制审批门禁——事件载荷携带分级供人审）。
// 返回**原始审批决定**（resolve payload 原文）；tool_result 包装由调用方负责
// （class 2 门禁需要原文判定 approved/rejected）。
func awaitApproval(ctx restate.Context, deps *Deps, in RunInput, runID string, step int, tc ToolCall, emit *Emitter, riskClass int) (string, error) {
	awakeable := restate.Awakeable[string](ctx)
	if err := deps.Sessions.SetPendingAwakeable(ctx, in.SessionID, awakeable.Id()); err != nil {
		return "", err
	}
	_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunAwaitingApproval, "await", "", map[string]any{
		"step": step, "awakeable_id": awakeable.Id(),
		"tool": tc.Name, "arguments": json.RawMessage(tc.Arguments),
		"risk_class": riskClass,
	})
	result, err := awakeable.Result() // 挂起：零进程占用，直到跨 HTTP resolve
	if err != nil {
		return "", err
	}
	_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunResumed, "resume", "", map[string]any{
		"step": step, "result": result,
	})
	return result, nil
}

// ensureSandbox 会话作用域沙箱懒创建（journaled：重放返回缓存 sandbox_id，不重复创建）
// 并把 sandbox_id 回填 session_object（AttachSandbox 幂等）。
func ensureSandbox(ctx restate.Context, deps *Deps, sessionID string, cfg sessionapi.AgentConfig) (string, error) {
	state, err := deps.Sessions.GetState(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if state.SandboxID != "" {
		return state.SandboxID, nil
	}
	spec := cfg.Environment.Sandbox
	image := spec.Image
	if image == "" {
		image = "python:3.12-slim" // 默认镜像（agent.config 未指定环境时）
	}
	sandboxID, err := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
		return deps.Executor.CreateSandbox(rc, execproto.CreateSandboxRequest{
			Image:     image,
			Limits:    execproto.Limits{CPU: spec.Limits["cpu"], Mem: spec.Limits["mem"], Disk: spec.Limits["disk"]},
			TTL:       spec.TTL,
			SessionID: sessionID,
		})
	}, restate.WithName("sandbox-create"))
	if err != nil {
		return "", err
	}
	// 回填会话状态（对象调用幂等；重放重发安全）
	if err := deps.Sessions.AttachSandbox(ctx, sessionID, sandboxID); err != nil {
		return "", err
	}
	return sandboxID, nil
}

// parseToolArguments 解析 tool_call.arguments：契约形态为 JSON 对象；
// 兼容历史字符串形态（JSON 字符串内嵌对象）。
func parseToolArguments(tc ToolCall) map[string]any {
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err == nil {
		return args
	}
	var raw string
	if err := json.Unmarshal(tc.Arguments, &raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &args)
	}
	return args
}

// codeToolInput 从 tool_call.arguments 提取执行输入（bash→command、run_python→code、
// list_files→path）。
func codeToolInput(tc ToolCall) string {
	args := parseToolArguments(tc)
	switch tc.Name {
	case runs.ToolBash:
		if v, ok := args["command"].(string); ok {
			return v
		}
	case runs.ToolRunPython:
		if v, ok := args["code"].(string); ok {
			return v
		}
	case runs.ToolListFiles:
		if v, ok := args["path"].(string); ok {
			return v
		}
	}
	raw, _ := json.Marshal(args)
	return string(raw)
}

// fileToolArgs 提取文件工具参数（path/content）。
func fileToolArgs(tc ToolCall) (path, content string) {
	args := parseToolArguments(tc)
	path, _ = args["path"].(string)
	content, _ = args["content"].(string)
	return path, content
}

func jsonToolResult(name string, res *ExecResult) string {
	return fmt.Sprintf(`{"name":%q,"result":{"exit":%d,"output":%s,"output_ref":%q,"truncated":%v}}`,
		name, res.Exit, mustJSONString(truncate(res.Output, 4096)), res.OutputRef, res.Truncated)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(截断)"
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
