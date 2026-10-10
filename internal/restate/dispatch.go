package restate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/wait"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/execproto"
)

// maxInlineBytes 内联载荷上限（契约规范 §7 journal 大小策略——超限外置
// RustFS；曾随 journal.go 误删，恢复为魔数锚点）。
const maxInlineBytes = 4096

// execOutcome 是 journaled 的 exec 结果（含执行时长；重放回放同一时长——确定性）。
// 字段必须导出：SDK 以 JSON 序列化 journal 条目，未导出字段重放后为零值（nil 解引用实证）。
type execOutcome struct {
	Result   *ExecResult   `json:"result"`
	Duration time.Duration `json:"duration_ns"`
}

// sandboxGone 判沙箱真实缺失（审计 P1-9：404 哨兵只限此类——此前把所有
// Execute/WriteFile 错误包装 404，恢复函数见 404 就清绑定重建重试——
// in-flight 409/权限拒绝/结果落库失败都被误判为「沙箱不存在」扩大重试）。
func sandboxGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, execproto.ErrSandboxNotFound) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "sandbox not found") || strings.Contains(s, "sandbox destroyed")
}

// execWithSandboxRecovery 执行沙箱操作；沙箱不存在（已销毁/回收）→ 清绑定 +
// 重建（快照恢复路径）→ 重试一次（评审 #7「一周前会话今天还能继续」的机制）。
func execWithSandboxRecovery(ctx restate.Context, deps *Deps, in RunInput, cfg sessionapi.AgentConfig, fn func(sandboxID string) error) error {
	sandboxID, err := ensureSandbox(ctx, deps, in.SessionID, cfg)
	if err != nil {
		return err
	}
	if err := fn(sandboxID); err != nil {
		// 哨兵识别：errors.Is 优先；文本兜底——Restate 的 ToTerminalError 只
		// 复制消息不保留错误链（SDK 文档实证），journal 重放的错误经包装后
		// errors.Is 失效——「sandbox not found」文本即 404 契约文本
		// 哨兵识别：① TerminalError code 404（SDK 研究结论：Run 错误统一
		// ToTerminalError 包装携带 code）② errors.Is 裸哨兵 ③ 文本兜底
		//（TerminalError 只保留 message——转换丢原始错误链，SDK 文档实证）
		if t := restate.AsTerminalError(err); t != nil && t.Code() == 404 {
			// 哨兵命中
		} else if !errors.Is(err, execproto.ErrSandboxNotFound) &&
			!strings.Contains(err.Error(), "sandbox not found") &&
			!strings.Contains(err.Error(), "sandbox destroyed") {
			return err
		}
		// 沙箱已被回收：清绑定 → 重建（带 snapshot_ref 恢复）→ 重试一次
		if err := deps.Sessions.ClearSandbox(ctx, in.SessionID); err != nil {
			return err
		}
		sandboxID, err = ensureSandbox(ctx, deps, in.SessionID, cfg)
		if err != nil {
			return err
		}
		return fn(sandboxID)
	}
	return nil
}

// dispatchTool 是四类工具路由（worker-架构设计 §1 dispatcher）的 W2 实现：
// 代码/命令类 → executor（本文件）；控制类（request_approval）→ W3 awakeable；
// MCP 类 → W6；API 类已在 harness 内联，不会到达这里。
// 返回 tool_result 内容（作为 tool 消息回喂模型）。
// dispatchTool 分流一个工具调用；返回 (结果 JSON 字符串, 计算秒, error)。
// 计算秒（沙箱执行时长）在 journaled Run 闭包内测量并随结果一起记账——
// 重放时回放同一时长（确定性），run 级预算熔断据此累加（边界语义 §1）。
func dispatchTool(ctx restate.Context, deps *Deps, in RunInput, runID string, step int, cfg sessionapi.AgentConfig, tc ToolCall, emit *Emitter) (string, float64, error) {
	switch {
	case tc.Name == runs.ToolBash || tc.Name == runs.ToolRunPython || tc.Name == runs.ToolListFiles:
		input, inputErr := codeToolInput(tc)
		if inputErr != nil {
			// 真实模型偶发空参调用（real-hitl 实证：批准后 arguments={}）——
			// 工具结果返回错误 JSON 供模型下一轮补参数（run 不失败；127
			// 防御保留——空命令不执行）
			return fmt.Sprintf(`{"name":%q,"result":{"exit":1,"error":%q}}`, tc.Name, inputErr.Error()), 0, nil
		}
		var outcome *execOutcome
		err := execWithSandboxRecovery(ctx, deps, in, cfg, func(sandboxID string) error {
			// 时长在 Run 闭包内测量并随结果 journal（重放回放同一时长）
			out, e := restate.Run(ctx, func(rc restate.RunContext) (*execOutcome, error) {
				started := time.Now()
				var res *ExecResult
				var err error
				for attempt := 0; attempt < 30; attempt++ {
					res, err = deps.Executor.Execute(rc, sandboxID, tc.Name, input,
						execproto.ExecuteIdempotencyKey(runID, step, tc.ID), runID)
					if err != nil && strings.Contains(err.Error(), "in-flight") {
						// 同键执行进行中（E2B 慢沙箱实证：执行窗口分钟级——
						// 3×2s 覆盖不住，terminal 失败）——10s 间隔等待后重试
						// （done 后回缓存——幂等无重复执行；总窗口 300s 对齐
						// E2B 命令超时 5 分钟）
						time.Sleep(10 * time.Second)
						continue
					}
					break
				}
				if err != nil {
					// 审计 P1-9：只对真实沙箱缺失标 404 哨兵——其他错误
					//（in-flight/权限/落库失败）原样 terminal（不触发重建）
					if sandboxGone(err) {
						return nil, restate.ToTerminalError(err, restate.WithErrorCode(404))
					}
					return nil, restate.ToTerminalError(err)
				}
				return &execOutcome{Result: res, Duration: time.Since(started)}, nil
			}, restate.WithName(StepName("exec", step, tc.ID)))
			if e != nil {
				return e
			}
			outcome = out
			return nil
		})
		if err != nil {
			return "", 0, err
		}
		res := outcome.Result
		state, _ := deps.Sessions.GetState(ctx, in.SessionID)
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.SandboxExec, "sandbox", tc.Name, map[string]any{
			"step": step, "tool": tc.Name, "exit": res.Exit, "sandbox_id": state.SandboxID,
			"output": truncate(res.Output, maxInlineBytes), "output_ref": res.OutputRef, "truncated": res.Truncated,
			"duration_ms": outcome.Duration.Milliseconds(), // 计算秒计量依据（W4）+ 预算熔断
		})
		return jsonToolResult(tc.Name, res), outcome.Duration.Seconds(), nil

	case tc.Name == runs.ToolWriteFile:
		path, content := fileToolArgs(tc)
		if path == "" {
			return "", 0, restate.ToTerminalError(fmt.Errorf("write_file 缺 path"))
		}
		// 经恢复链（评审 #7 同款）：沙箱已销毁 → 404 哨兵 → 清绑定 → 重建
		//（blob 合同 e2e 实证：不经恢复会 500 循环旧容器）
		err := execWithSandboxRecovery(ctx, deps, in, cfg, func(sandboxID string) error {
			_, wErr := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
				// TerminalError 包装（#2 修复纪律：裸错误 → Infinite 重试循环）
				if e := deps.Executor.WriteFile(rc, sandboxID, path, content); e != nil {
					if sandboxGone(e) {
						return "", restate.ToTerminalError(e, restate.WithErrorCode(404))
					}
					return "", restate.ToTerminalError(e)
				}
				return "", nil
			}, restate.WithName(StepName("exec", step, tc.ID)))
			return wErr
		})
		if err != nil {
			return "", 0, err
		}
		state, _ := deps.Sessions.GetState(ctx, in.SessionID)
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.SandboxExec, "sandbox", tc.Name, map[string]any{
			"step": step, "tool": tc.Name, "exit": 0, "path": path, "sandbox_id": state.SandboxID,
		})
		return fmt.Sprintf(`{"name":%q,"result":{"written":%q}}`, tc.Name, path), 0, nil

	case tc.Name == runs.ToolReadFile:
		path, _ := fileToolArgs(tc)
		if path == "" {
			return "", 0, restate.ToTerminalError(fmt.Errorf("read_file 缺 path"))
		}
		var content string
		err := execWithSandboxRecovery(ctx, deps, in, cfg, func(sb string) error {
			result, e := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
				content, rErr := deps.Executor.ReadFile(rc, sb, path)
				if rErr != nil {
					// 审计准入 #6：只对真实沙箱缺失标 404 哨兵（权限/网络/
					// 存储错误不得清绑定重建）
					if sandboxGone(rErr) {
						return "", restate.ToTerminalError(rErr, restate.WithErrorCode(404))
					}
					return "", restate.ToTerminalError(rErr)
				}
				return content, nil
			}, restate.WithName(StepName("exec", step, tc.ID)))
			if e != nil {
				return e
			}
			content = result
			return nil
		})
		if err != nil {
			return "", 0, err
		}
		state, _ := deps.Sessions.GetState(ctx, in.SessionID)
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.SandboxExec, "sandbox", tc.Name, map[string]any{
			"step": step, "tool": tc.Name, "exit": 0, "path": path, "sandbox_id": state.SandboxID,
		})
		return fmt.Sprintf(`{"name":%q,"result":{"content":%s}}`, tc.Name, mustJSONString(truncate(content, maxInlineBytes))), 0, nil

	case tc.Name == runs.ToolSpawnSubagent:
		// 子 Agent：建子会话 + child run_workflow（durable 等待）+ 结果回喂（W6）
		return dispatchSubagent(ctx, deps, in, runID, step, cfg, tc, emit)

	case strings.HasPrefix(tc.Name, runs.MCPToolPrefix):
		// MCP 工具（落地方案 §11：worker 托管客户端调用；harness 零状态）
		return dispatchMCP(ctx, deps, in, runID, step, tc, emit)

	case tc.Name == runs.ToolNextSpeaker:
		// 群聊：moderator 指定发言者 → 成员 child run（durable 等待）→ 回喂（W7）
		state, err := deps.Sessions.GetState(ctx, in.SessionID)
		if err != nil {
			return "", 0, err
		}
		return speakAsParticipant(ctx, deps, in, runID, step, state, tc, emit)

	case tc.Name == runs.ToolRequestApproval:
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
	digest := approvalDigest(runID, step, tc)
	// 期 6 ①：operator_input 等待注册 intent/expect（为什么停 + 期待条件——
	// 恢复时「上次为什么停」进 causal slice）
	_ = deps.Store.RegisterWait(ctx, in.SessionID, wait.OperatorInput, awakeable.Id(),
		fmt.Sprintf("审批请求：%s", tc.Name), "expect=approve|reject")

	if err := deps.Sessions.SetPendingAwakeable(ctx, in.SessionID, awakeable.Id(), digest, tc.Name); err != nil {
		// 审计 #8 修复后 GitHub 实证：非 terminal 错误 → SDK 重试 → 新
		// awakeable id → 批准打到旧槽 → 新挂起无人批 → 拒绝级联。
		// terminal 化：重试链断（批准路径的失败显式暴露而非静默换槽）。
		return "", restate.ToTerminalError(err)
	}
	_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunAwaitingApproval, "await", "", map[string]any{
		"step": step, "awakeable_id": awakeable.Id(),
		"tool": tc.Name, "arguments": json.RawMessage(tc.Arguments),
		"risk_class": riskClass, "action_digest": digest,
	})
	// D 批：挂起即冻结——算力即时释放（不再依赖 10m lease+5m GC+TTL 存在）
	if state, sErr := deps.Sessions.GetState(ctx, in.SessionID); sErr == nil && state.SandboxID != "" {
		_ = deps.Executor.FreezeSandbox(ctx, state.SandboxID)
	}
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
	// Tier2 快照恢复（评审 #7）：旧沙箱行 snapshot_ref 存在 → 从快照重建
	//（镜像 + 卷内容；「一周前会话今天还能继续」的机制）
	restoreFrom := ""
	if prev, err := deps.Store.GetSandboxBySession(ctx, sessionID); err == nil && prev.SnapshotRef != nil && *prev.SnapshotRef != "" {
		restoreFrom = *prev.SnapshotRef
	}
	// blob 合同（期 2 §A）：无快照但有工作区索引 → blob: 恢复（快照之外
	// 第二条恢复链——销毁无快照也能重建；fork 空间面懒恢复的基础）
	if restoreFrom == "" {
		if files, fErr := deps.Store.ListWorkspaceFiles(ctx, sessionID, 1); fErr == nil && len(files) > 0 {
			restoreFrom = "blob:"
		}
	}
	sandboxID, err := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
		return deps.Executor.CreateSandbox(rc, execproto.CreateSandboxRequest{
			Image:       image,
			Limits:      execproto.Limits{CPU: spec.Limits["cpu"], Mem: spec.Limits["mem"], Disk: spec.Limits["disk"]},
			TTL:         spec.TTL,
			SessionID:   sessionID,
			RestoreFrom: restoreFrom,
			Driver:      spec.Driver, // 期 4 §C：租户档路由
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

// approvalDigest 待审批动作摘要（评审 #5：精确绑定 run/step/工具/参数）。
func approvalDigest(runID string, step int, tc ToolCall) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s:%s", runID, step, tc.Name, string(tc.Arguments))))
	return hex.EncodeToString(sum[:8])
}

// codeToolInput 从 tool_call.arguments 提取执行输入（bash→command、run_python→code、
// list_files→path）。
// codeToolInput 提取代码类工具输入；参数缺失返回明确错误——真实模型偶发
// 空参调用（arguments={}）曾把 "{}" 当命令执行（127 实证——生产形态基准）。
func codeToolInput(tc ToolCall) (string, error) {
	args := parseToolArguments(tc)
	switch tc.Name {
	case runs.ToolBash:
		if v, ok := args["command"].(string); ok && strings.TrimSpace(v) != "" {
			return v, nil
		}
		return "", fmt.Errorf("工具 %s 参数缺失：需要非空 command（收到 %s）", tc.Name, compactArgs(args))
	case runs.ToolRunPython:
		if v, ok := args["code"].(string); ok && strings.TrimSpace(v) != "" {
			return v, nil
		}
		return "", fmt.Errorf("工具 %s 参数缺失：需要非空 code（收到 %s）", tc.Name, compactArgs(args))
	case runs.ToolListFiles:
		if v, ok := args["path"].(string); ok && strings.TrimSpace(v) != "" {
			return v, nil
		}
		return "", fmt.Errorf("工具 %s 参数缺失：需要非空 path（收到 %s）", tc.Name, compactArgs(args))
	}
	return "", fmt.Errorf("未知代码工具 %s", tc.Name)
}

// compactArgs 参数缺失错误里的短形（诊断上下文）。
func compactArgs(args map[string]any) string {
	raw, _ := json.Marshal(args)
	if len(raw) > 200 {
		raw = raw[:200]
	}
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
