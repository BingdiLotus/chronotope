package restate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// maxSteps 是 MVP 防失控上限（边界语义 §1 的 无进展检测/预算熔断 在其内触发）。
const maxSteps = 16

// noProgressStreak 是「连续指纹不变轮数」阈值（边界语义 §1：N=5 → run.failed{no_progress}）。
const noProgressStreak = 5

// budgetKeys 是 run 级预算键（AgentConfig.Budget；边界语义 §1：累加 token/计算秒）。
const (
	budgetKeyTokens  = "max_tokens"
	budgetKeyCompute = "max_compute_seconds"
)

// parseBudget 解析预算（未知键/非法值忽略——契约向后兼容）。
func parseBudget(b map[string]any) (maxTokens, maxCompute float64) {
	asFloat := func(k string) float64 {
		if v, ok := b[k]; ok {
			if f, ok := v.(float64); ok && f > 0 {
				return f
			}
		}
		return 0
	}
	return asFloat(budgetKeyTokens), asFloat(budgetKeyCompute)
}

// toolFingerprint 计算工具调用指纹（无进展检测；边界语义 §1：hash(工具+参数)）。
func toolFingerprint(name string, args json.RawMessage) string {
	h := sha256.Sum256([]byte(name + "\x00" + string(args)))
	return hex.EncodeToString(h[:])
}

// RunInput 是 run_workflow 的输入（api 经 Restate ingress 提交）。
type RunInput struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
	Topic     string `json:"topic,omitempty"` // 记忆 topic 标签（分层记忆；默认 default）
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
		"input": in.Input, "model": cfg.Model, "topic": topicOf(in),
	}); err != nil {
		return RunOutput{}, restate.ToTerminalError(err)
	}
	// run 状态行记账（幂等）：worker 是终态记账者，api 崩溃后状态仍收敛
	_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunRunning)

	// 三级熔断 ②：org 日预算入口检查 → 超限冻结（挂起等待充值，零成本不杀 run）
	if sess, sErr := deps.Store.GetSession(ctx, in.SessionID); sErr == nil {
		if exceeded, key := orgQuotaExceeded(ctx, deps, sess.OrgID, time.Now()); exceeded {
			if err := freezeRun(ctx, deps, in, runID, emit, key); err != nil {
				return RunOutput{}, restate.ToTerminalError(fmt.Errorf("freeze: %w", err))
			}
		}
	}

	msgs, err := buildMessages(ctx, deps.Store, in, cfg)
	if err != nil {
		return RunOutput{}, restate.ToTerminalError(err)
	}
	// 用户输入写回消息全量（对话真相；buildMessages 之后写，避免本轮历史重复注入）
	if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, 0, "user", json.RawMessage(mustJSONString(in.Input))); err != nil {
		return RunOutput{}, restate.ToTerminalError(err)
	}

	maxTokens, maxCompute := parseBudget(cfg.Budget)
	var accTokens int64
	var accCompute float64
	lastFingerprint := ""
	streak := 0

	failBudget := func(step int, key string) (RunOutput, error) {
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.BudgetExceeded, "budget", "", map[string]any{
			"run_id": runID, "key": key,
		})
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "", "", map[string]any{
			"reason": "budget_exceeded", "key": key,
		})
		_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
		return RunOutput{}, restate.ToTerminalError(fmt.Errorf("budget exceeded: %s", key))
	}
	failNoProgress := func(step int) (RunOutput, error) {
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "", "", map[string]any{
			"reason": "no_progress", "streak": noProgressStreak,
		})
		_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
		return RunOutput{}, restate.ToTerminalError(fmt.Errorf("no progress: %d 轮指纹不变", noProgressStreak))
	}

	for step := 0; step < maxSteps; step++ {
		req := &runs.Request{
			Protocol:       runs.ProtocolVersion,
			RunID:          runID,
			SessionID:      in.SessionID,
			Step:           step,
			Model:          cfg.Model,
			Messages:       msgs,
			Tools:          groupAwareTools(cfg, state),
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
			_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
			return RunOutput{}, runErr
		}

		if err := emit.Emit(ctx, in.SessionID, runID, step, event.LLMCall, "llm", "", map[string]any{
			"step": step, "usage": res.Usage,
			"msgs": len(req.Messages), // 组装消息数（分层记忆注入的 e2e 可观测性）
		}); err != nil {
			return RunOutput{}, restate.ToTerminalError(err)
		}

		// 预算累加：token（每次 harness 调用后；边界语义 §1 run 级预算）
		accTokens += int64(res.Usage.TokensIn + res.Usage.TokensOut)
		if maxTokens > 0 && float64(accTokens) > maxTokens {
			return failBudget(step, budgetKeyTokens)
		}
		// 三级熔断 ②：org 日预算每步检查 → 超限冻结（充值后继续本 run）
		if sess, sErr := deps.Store.GetSession(ctx, in.SessionID); sErr == nil {
			if exceeded, key := orgQuotaExceeded(ctx, deps, sess.OrgID, time.Now()); exceeded {
				if err := freezeRun(ctx, deps, in, runID, emit, key); err != nil {
					return RunOutput{}, restate.ToTerminalError(fmt.Errorf("freeze: %w", err))
				}
			}
		}

		if res.ErrCode != "" { // harness 失败终态（error 帧）
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "", "", map[string]any{
				"code": res.ErrCode, "message": res.ErrMsg,
			})
			_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
			return RunOutput{}, restate.ToTerminalError(fmt.Errorf("harness: %s: %s", res.ErrCode, res.ErrMsg))
		}

		if len(res.ToolCalls) > 0 {
			// 工具分流（worker-架构设计 §1 dispatcher）：代码/命令类 → executor（W2）；
			// 控制类（request_approval）→ W3 awakeable；API 类已在 harness 内联。
			for _, tc := range res.ToolCalls {
				_ = emit.Emit(ctx, in.SessionID, runID, step, event.ToolCall, "tool", tc.Name, map[string]any{
					"step": step, "id": tc.ID, "name": tc.Name, "arguments": json.RawMessage(tc.Arguments),
				})
				// class 2 强制审批门禁（边界语义 §2：永不自动执行）——
				// 挂起 → 批准则照常 dispatch；拒绝 → audit.tool_denied + run.failed{tool_denied}
				if riskClassOf(tc.Name, cfg) >= 2 {
					decision, err := awaitApproval(ctx, deps, in, runID, step, tc, emit, 2)
					if err != nil {
						return RunOutput{}, restate.ToTerminalError(fmt.Errorf("await approval (class 2): %w", err))
					}
					if !approvalGranted(decision) {
						_ = emit.Emit(ctx, in.SessionID, runID, step, event.AuditToolDenied, "deny", tc.Name, map[string]any{
							"step": step, "tool": tc.Name, "risk_class": 2,
						})
						_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "", "", map[string]any{
							"reason": "tool_denied", "tool": tc.Name,
						})
						_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
						return RunOutput{}, restate.ToTerminalError(fmt.Errorf("工具 %s 的 class 2 审批被拒绝", tc.Name))
					}
				}
				// 无进展检测：连续指纹不变 → 熔断（dispatch 前，避免第 N 次重复执行）
				fp := toolFingerprint(tc.Name, tc.Arguments)
				if fp == lastFingerprint {
					streak++
				} else {
					lastFingerprint = fp
					streak = 1
				}
				if streak >= noProgressStreak {
					return failNoProgress(step)
				}
				result, computeSeconds, err := dispatchTool(ctx, deps, in, runID, step, cfg, tc, emit)
				if err != nil {
					return RunOutput{}, restate.ToTerminalError(
						fmt.Errorf("dispatch %s (step %d): %w", tc.Name, step, err))
				}
				accCompute += computeSeconds
				if maxCompute > 0 && accCompute > maxCompute {
					return failBudget(step, budgetKeyCompute)
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
			_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunCompleted)
			// 记忆消化（run 结束后；失败不影响主流程——内部已吞错）
			_ = consolidate(ctx, deps, in.SessionID, runID, topicOf(in), emit)
			return RunOutput{Final: res.Final, Steps: step + 1}, nil
		}
	}

	_ = emit.Emit(ctx, in.SessionID, runID, maxSteps, event.RunFailed, "", "", map[string]any{
		"reason": "max_steps",
	})
	_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
	return RunOutput{}, restate.ToTerminalError(fmt.Errorf("max steps (%d) exceeded", maxSteps))
}

// buildMessages 组装本轮消息（分层记忆的 W5 前简化形态：system 指令 + 历史 + 本 run 输入）。
func buildMessages(ctx context.Context, st Store, in RunInput, cfg sessionapi.AgentConfig) ([]runs.Message, error) {
	history, err := st.ListMessages(ctx, in.SessionID, 50)
	if err != nil {
		return nil, err
	}
	msgs := []runs.Message{{Role: "system", Content: cfg.Instructions, Source: "trusted"}}
	// 分层记忆注入（边界语义 §7 组装函数）：主题滚动摘要 + 检索片段（topic 作用域优先）
	topic := topicOf(in)
	if sum, err := st.LatestSummary(ctx, in.SessionID, topic); err == nil && sum != nil {
		msgs = append(msgs, runs.Message{Role: "system", Content: "【主题摘要】" + sum.Summary, Source: "trusted"})
	}
	if items, err := st.ListMemoryItems(ctx, in.SessionID, topic, memoryRetrieveK); err == nil && len(items) > 0 {
		var b strings.Builder
		b.WriteString("【相关记忆】")
		for _, it := range items {
			b.WriteString("\n- " + it.Content)
		}
		msgs = append(msgs, runs.Message{Role: "system", Content: b.String(), Source: "trusted"})
	}
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

// groupAwareTools 组装工具清单：群聊会话（participants 非空）附加主持工具
// next_speaker（moderator 决策发言顺序；落地方案 §14）。
func groupAwareTools(cfg sessionapi.AgentConfig, state SessionState) []runs.Tool {
	tools := toolsFromConfig(cfg)
	if len(state.Participants) > 0 {
		tools = append(tools, groupTools()...)
	}
	return tools
}

// toolsFromConfig 把 agent.config.tools 映射为 /runs 协议的工具清单（风险分级 W5 细化）。
func toolsFromConfig(cfg sessionapi.AgentConfig) []runs.Tool {
	tools := make([]runs.Tool, 0, len(cfg.Tools))
	for _, name := range cfg.Tools {
		if !runs.IsVocabularyName(name) {
			continue // 契约硬约束：非法工具名不发往 harness
		}
		tools = append(tools, runs.Tool{Type: "function", Name: name, RiskClass: riskClassOf(name, cfg)})
	}
	return tools
}
