package restate

import (
	"context"
	"encoding/json"
	"fmt"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// maxSteps 是 MVP 防失控上限（无进展检测/三级熔断在 W5 完整落地，边界语义 §1）。
const maxSteps = 16

// RunInput 是 run_workflow 的输入（api 经 Restate ingress 提交）。
type RunInput struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
}

// RunOutput 是 run_workflow 的结果摘要（journal 只存摘要，契约规范 §7）。
type RunOutput struct {
	Final string `json:"final"`
	Steps int    `json:"steps"`
}

// runWorkflowDef 注册任务编排 workflow（key=run_id；agent 主循环，worker-架构设计 §3）。
func runWorkflowDef(deps *Deps) restate.ServiceDefinition {
	return restate.NewWorkflow(RunWorkflowName).
		Handler("run", restate.NewWorkflowHandler[RunInput, RunOutput](func(ctx restate.WorkflowContext, in RunInput) (RunOutput, error) {
			return runLoop(ctx, deps, in, restate.Key(ctx))
		}))
}

// runLoop 是 agent 主循环（ctx 收敛到 restate.Context + 显式 runID，便于单测：
// runID 生产上来自 workflow key，与输入幂等键一致——契约规范 §6 幂等链）。
func runLoop(ctx restate.Context, deps *Deps, in RunInput, runID string) (RunOutput, error) {
	emit := &Emitter{Store: deps.Store}

	// 会话状态：从 session_object 读（单写者串行；AgentConfig 含模型/工具/版本）
	state, err := deps.Sessions.GetState(ctx, in.SessionID)
	if err != nil {
		return RunOutput{}, err
	}
	if state.AgentConfig.Model == "" {
		return RunOutput{}, restate.ToTerminalError(
			fmt.Errorf("session %s has no agent config（session_object 未初始化）", in.SessionID))
	}
	cfg := state.AgentConfig

	if err := emit.Emit(ctx, in.SessionID, runID, 0, event.RunStarted, "", "", map[string]any{
		"input": in.Input, "model": cfg.Model,
	}); err != nil {
		return RunOutput{}, restate.ToTerminalError(err)
	}

	msgs, err := buildMessages(ctx, deps.Store, in, cfg)
	if err != nil {
		return RunOutput{}, restate.ToTerminalError(err)
	}

	for step := 0; step < maxSteps; step++ {
		req := &runs.Request{
			Protocol:       runs.ProtocolVersion,
			RunID:          runID,
			SessionID:      in.SessionID,
			Step:           step,
			Model:          cfg.Model,
			Messages:       msgs,
			Tools:          toolsFromConfig(cfg),
			MaxTurns:       8,
			MaxOutputBytes: 524288,
		}

		// 一次 harness 调用 = 一个 journaled step（缓存键 = journal 位置，
		// 崩溃重放直接回放缓存，不重调 harness——三条纪律之三）
		res, runErr := restate.Run(ctx, func(rc restate.RunContext) (*Result, error) {
			return deps.Harness.Call(rc, req)
		}, restate.WithName(StepName("harness", step, "")))
		if runErr != nil {
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "harness", "", map[string]any{
				"reason": runErr.Error(),
			})
			return RunOutput{}, runErr
		}

		if err := emit.Emit(ctx, in.SessionID, runID, step, event.LLMCall, "llm", "", map[string]any{
			"step": step, "usage": res.Usage,
		}); err != nil {
			return RunOutput{}, restate.ToTerminalError(err)
		}

		if res.ErrCode != "" { // harness 失败终态（error 帧）
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "", "", map[string]any{
				"code": res.ErrCode, "message": res.ErrMsg,
			})
			return RunOutput{}, restate.ToTerminalError(fmt.Errorf("harness: %s: %s", res.ErrCode, res.ErrMsg))
		}

		if len(res.ToolCalls) > 0 {
			// 工具分流（worker-架构设计 §1 dispatcher）：代码/命令类 → executor（W2）；
			// 控制类（request_approval）→ W3 awakeable；API 类已在 harness 内联。
			for _, tc := range res.ToolCalls {
				_ = emit.Emit(ctx, in.SessionID, runID, step, event.ToolCall, "tool", tc.Name, map[string]any{
					"step": step, "id": tc.ID, "name": tc.Name, "arguments": json.RawMessage(tc.Arguments),
				})
				result, err := dispatchTool(ctx, deps, in, runID, step, cfg, tc, emit)
				if err != nil {
					return RunOutput{}, restate.ToTerminalError(
						fmt.Errorf("dispatch %s (step %d): %w", tc.Name, step, err))
				}
				content, _ := json.Marshal(result)
				if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, step, "tool", content); err != nil {
					return RunOutput{}, restate.ToTerminalError(err)
				}
				msgs = append(msgs, runs.Message{Role: "tool", Content: result, Source: "sandbox"})
			}
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.StepJournaled, "step", "", nil)
			continue
		}

		if res.Done {
			content, _ := json.Marshal(res.Final)
			if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, step, "assistant", content); err != nil {
				return RunOutput{}, restate.ToTerminalError(err)
			}
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunCompleted, "", "", map[string]any{
				"final": res.Final, "steps": step + 1, "truncated": res.Truncated,
			})
			return RunOutput{Final: res.Final, Steps: step + 1}, nil
		}
	}

	_ = emit.Emit(ctx, in.SessionID, runID, maxSteps, event.RunFailed, "", "", map[string]any{
		"reason": "max_steps",
	})
	return RunOutput{}, restate.ToTerminalError(fmt.Errorf("max steps (%d) exceeded", maxSteps))
}

// buildMessages 组装本轮消息（分层记忆的 W5 前简化形态：system 指令 + 历史 + 本 run 输入）。
func buildMessages(ctx context.Context, st Store, in RunInput, cfg sessionapi.AgentConfig) ([]runs.Message, error) {
	history, err := st.ListMessages(ctx, in.SessionID, 50)
	if err != nil {
		return nil, err
	}
	msgs := []runs.Message{{Role: "system", Content: cfg.Instructions, Source: "trusted"}}
	for _, m := range history {
		var text string
		if err := json.Unmarshal(m.Content, &text); err != nil {
			// 非纯文本历史（如结构化 tool_result 字符串）：跳过组装，避免破坏消息形状
			continue
		}
		msgs = append(msgs, runs.Message{Role: m.Role, Content: text, Source: "trusted"})
	}
	msgs = append(msgs, runs.Message{Role: "user", Content: in.Input})
	return msgs, nil
}

// toolsFromConfig 把 agent.config.tools 映射为 /runs 协议的工具清单（风险分级 W5 细化）。
func toolsFromConfig(cfg sessionapi.AgentConfig) []runs.Tool {
	tools := make([]runs.Tool, 0, len(cfg.Tools))
	for _, name := range cfg.Tools {
		if !runs.IsVocabularyName(name) {
			continue // 契约硬约束：非法工具名不发往 harness
		}
		tools = append(tools, runs.Tool{Type: "function", Name: name, RiskClass: 1})
	}
	return tools
}
