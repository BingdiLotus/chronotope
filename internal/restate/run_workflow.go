package restate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/policy"
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
	Final    string `json:"final"`
	Steps    int    `json:"steps"`
	Canceled bool   `json:"canceled,omitempty"`
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
func runLoop(ctx restate.Context, deps *Deps, in RunInput, runID string) (out RunOutput, err error) {
	defer func() {
		if r := recover(); r != nil {
			// GitHub 诊断：panic 堆栈（recover 打栈后重抛——SDK 只记 error
			// 不记堆栈，无法定位）
			slog.Default().Error("runLoop panic", "run", runID, "recover", fmt.Sprint(r),
				"stack", string(debug.Stack()))
			panic(r)
		}
	}()
	emit := &Emitter{Store: deps.Store}

	// 会话状态：从 session_object 读（单写者串行；AgentConfig 含模型/工具/版本）
	state, err := deps.Sessions.GetState(ctx, in.SessionID)
	if err != nil {
		return RunOutput{}, err
	}
	if state.AgentConfig.Model == "" {
		return RunOutput{}, restate.ToTerminalError(
			fmt.Errorf("session %s has no agent config（session state not initialized）", in.SessionID))
	}
	// 版本绑定（正确性二期 ⑩）：run 绑定启动时的 config 快照——agent 升级/
	// 会话 config 变化不改变本 run 的后续步骤（run 内确定性）；旧 run 无快照
	//（bound 缺失）回退会话 config 兼容。
	cfg := state.AgentConfig
	if runRow, gErr := deps.Store.GetRun(ctx, runID); gErr == nil && runRow != nil && len(runRow.Bound) > 0 {
		raw, mErr := json.Marshal(runRow.Bound)
		if mErr == nil {
			var bound struct {
				AgentConfig sessionapi.AgentConfig `json:"agent_config"`
			}
			if uErr := json.Unmarshal(raw, &bound); uErr == nil && bound.AgentConfig.Model != "" {
				cfg = bound.AgentConfig
			}
		}
	}

	if err := emit.Emit(ctx, in.SessionID, runID, 0, event.RunStarted, "", "", map[string]any{
		"input": in.Input, "model": cfg.Model, "topic": topicOf(in),
	}); err != nil {
		return RunOutput{}, restate.ToTerminalError(err)
	}
	// run 状态行记账（幂等）：worker 是终态记账者，api 崩溃后状态仍收敛
	_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunRunning)

	// 三级熔断 ②：org 日预算入口快照（正确性二期 ⑧——journal 化，run 内检查
	// 只基于快照+累计；重放回放同一快照，执行中其他会话推进用量不影响分支）
	var budgetSnap policy.BudgetSnapshot
	if sess, sErr := deps.Store.GetSession(ctx, in.SessionID); sErr == nil {
		// 预算策略缝（期 3 §A）：快照 journal 化（⑧ 确定性）+ 策略判定——
		// 默认 AllowAll（无业务=无限）；参考实现 OrgDailyBudget 平移自 org 日预算
		budgetSnap, err = restate.Run(ctx, func(rc restate.RunContext) (policy.BudgetSnapshot, error) {
			return budgetSnapshotOf(rc, deps.BudgetPolicy, sess.OrgID), nil
		}, restate.WithName("budget-snapshot"))
		if err != nil {
			budgetSnap = policy.BudgetSnapshot{TenantID: sess.OrgID} // fail-open
		}
		if d := budgetCheck(ctx, deps.BudgetPolicy, budgetSnap, policy.Accum{}); !d.Allow {
			if err := freezeRun(ctx, deps, in, runID, emit, d.Key); err != nil {
				return RunOutput{}, restate.ToTerminalError(fmt.Errorf("freeze: %w", err))
			}
			// 解冻 = 充值后继续：重新快照（journaled）——充值后的配额对后续检查生效
			budgetSnap, err = restate.Run(ctx, func(rc restate.RunContext) (policy.BudgetSnapshot, error) {
				return budgetSnapshotOf(rc, deps.BudgetPolicy, sess.OrgID), nil
			}, restate.WithName("budget-snapshot"))
			if err != nil {
				budgetSnap = policy.BudgetSnapshot{TenantID: sess.OrgID}
			}
		}
	}

	// 用户输入写回消息全量（对话真相，每 run 一次；buildMessages 之后读回——
	// 循环内重复写回会随轮次复读输入，真实模型 e2e 实证 400）
	if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, 0, "user", json.RawMessage(mustJSONString(in.Input))); err != nil {
		return RunOutput{}, restate.ToTerminalError(err)
	}
	// 审计 #5：请求冻结——整个组装 journaled（重放回放缓存的消息切片，
	// 不再重读 PG 历史/embed/知识检索/摘要/记忆——崩溃后外围更新不漂移
	// 同一未确认 step 的 ModelRequest）
	msgs, err := restate.Run(ctx, func(rc restate.RunContext) ([]runs.Message, error) {
		return buildMessages(rc, deps.Store, deps.Harness, in, runID, cfg, state.Skills)
	}, restate.WithName("build-messages"))
	if err != nil {
		return RunOutput{}, restate.ToTerminalError(err)
	}

	maxTokens, maxCompute := parseBudget(cfg.Budget)
	var accTokens int64
	var accCompute float64
	lastFingerprint := ""
	streak := 0
	groupTurns := 0
	var leaseSandboxID string // 沙箱租约绑定（首次 ensureSandbox 后设置）
	var leaseGen int64
	// 终态释放（journaled 幂等：代次不符/已释放忽略；重放回放同释放）
	releaseLease := func() {
		if leaseSandboxID != "" && leaseGen > 0 {
			_, _ = restate.Run(ctx, func(rc restate.RunContext) (struct{}, error) {
				return struct{}{}, deps.Executor.ReleaseLease(rc, leaseSandboxID, leaseGen)
			}, restate.WithName("lease-release"))
		}
	}

	failBudget := func(step int, key string) (RunOutput, error) {
		releaseLease()
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
		releaseLease()
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "", "", map[string]any{
			"reason": "no_progress", "streak": noProgressStreak,
		})
		_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
		return RunOutput{}, restate.ToTerminalError(fmt.Errorf("no progress: %d 轮指纹不变", noProgressStreak))
	}

	for step := 0; step < maxSteps; step++ {
		// 取消检查点（评审 #6 非抢占式：每步 harness 调用前检查会话取消标志）
		stepState, err := deps.Sessions.GetState(ctx, in.SessionID)
		// D 批：paused 检查点——暂停即冻结沙箱（算力即时释放；pause 不再是
		// 状态值——运行中的 run 在检查点冻结并终止）
		if err == nil && stepState.Phase == sessionapi.PhasePaused {
			if stepState.SandboxID != "" {
				_ = deps.Executor.FreezeSandbox(ctx, stepState.SandboxID)
			}
			_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
			return RunOutput{}, restate.ToTerminalError(fmt.Errorf("会话已暂停（沙箱已冻结）"))
		}
		if err == nil && stepState.CancelRequested {
			// 取消终态：绑定租约（若有）后释放——旧持有者代次随释放失活
			if stepState.SandboxID != "" && leaseSandboxID == "" {
				leaseSandboxID = stepState.SandboxID
			}
			releaseLease()
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunCanceled, "", "", map[string]any{
				"step": step,
			})
			_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunCanceled)
			_ = deps.Sessions.ClearCancel(ctx, in.SessionID) // run 级一次性——审计 #9
			return RunOutput{Final: "已取消", Steps: step + 1, Canceled: true}, nil
		}
		// 沙箱租约绑定（会话沙箱懒创建后生效；重放时 stepState 同值）
		if stepState.SandboxID != "" && stepState.SandboxID != leaseSandboxID {
			leaseSandboxID = stepState.SandboxID
			leaseGen = 0 // 新绑定重置代次（首次续约取得）
		}
		// ComputeLease 续约（正确性二期 ⑨）：每步 harness 调用前 journaled 续约——
		// run 活着即持有；挂起（审批/冻结）期间进程挂起不续约 → 租约过期 →
		// GC 可回收沙箱（依赖安全：挂起零成本），解冻后走 lease-expired 重建
		//（快照恢复路径，评审 #7 衔接）。重放回放同一次续约（journaled）。
		if leaseSandboxID != "" {
			if gen, lErr := restate.Run(ctx, func(rc restate.RunContext) (int64, error) {
				return deps.Executor.AcquireLease(rc, leaseSandboxID, runID, "10m")
			}, restate.WithName("lease-renew")); lErr == nil {
				leaseGen = gen
			}
		}

		// 群聊决策护栏：轮次用尽后移除 next_speaker，强制 moderator 终答
		stepTools := groupAwareTools(cfg, state)
		if groupTurns >= maxGroupTurns {
			stepTools = toolsFromConfig(cfg)
		}
		// MCP 工具（落地方案 §11：worker 托管客户端，harness 只见 schema）：
		// 懒 tools/list（失败降级跳过该 server，不阻断 run）
		// MCP 网关 allowlist（期 3 §C）：按会话租户过滤工具（无行 = 全拒）
		mcpTools, mcpErr := mcpToolsFromState(ctx, deps.MCP, state.MCP, func(server, tool string) (bool, error) {
			sess, sErr := deps.Store.GetSession(ctx, in.SessionID)
			if sErr != nil {
				return false, nil
			}
			return deps.Store.MCPToolAllowed(ctx, sess.OrgID, server, tool)
		})
		if mcpErr != nil {
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.EventTruncated, "mcp", "tools", map[string]any{
				"step": step, "error": mcpErr.Error(),
			}) // 降级：该 server 工具本步不可用，不阻断 run
		} else {
			stepTools = append(stepTools, mcpTools...)
		}
		req := &runs.Request{
			Protocol:       runs.ProtocolVersion,
			RunID:          runID,
			SessionID:      in.SessionID,
			Step:           step,
			Model:          cfg.Model,
			Messages:       msgs,
			Tools:          stepTools,
			MaxTurns:       8,
			MaxOutputBytes: 524288,
		}

		// 一次 harness 调用 = 一个 journaled step（缓存键 = journal 位置，
		// 崩溃重放直接回放缓存，不重调 harness——三条纪律之三）
		res, runErr := restate.Run(ctx, func(rc restate.RunContext) (*Result, error) {
			// 审计 4.1 仲裁合同：账本写入失败 = 零次新派发；冲突读既存状态
			reqHash := requestHashOf(req)
			row, err := deps.Store.PutLLMCallPrepared(rc, runID, step, reqHash)
			if err != nil {
				return nil, restate.ToTerminalError(fmt.Errorf("llm 账本 prepare 失败（零派发）: %w", err))
			}
			if row == nil {
				return nil, restate.ToTerminalError(fmt.Errorf("llm 账本 prepare 无行（零派发）"))
			}
			if row.State == "result" {
				// 合同表：同 operation 已有结果——hash 同 → 返回冻结原结果
				//（不执行）；hash 异 → 拒绝（operation 语义冲突）
				if row.RequestHash != "" && row.RequestHash != reqHash {
					return nil, restate.ToTerminalError(fmt.Errorf("llm 账本结果与请求 hash 不符（operation 冲突）"))
				}
				if row.Result != "" {
					var cached Result
					if jErr := json.Unmarshal([]byte(row.Result), &cached); jErr == nil {
						return &cached, nil // 原结果——零派发
					}
				}
				return nil, restate.ToTerminalError(fmt.Errorf("llm 账本已有结果（无法回读——人工裁决）"))
			}
			if row.State == "unknown" {
				// 合同表：已派发但生效未知——无法查询 receipt 时停住（人工裁决）
				return nil, restate.ToTerminalError(fmt.Errorf("llm 账本 unknown 停派发（需人工裁决）"))
			}
			// 审计 4.1：prepared/dispatched 的 hash 校验（异 hash 拒绝——同一
			// 不可变请求的绑定）；dispatched 无接受证据 → 不重派发（停住对账）
			if row.State == "dispatched" {
				return nil, restate.ToTerminalError(fmt.Errorf("llm 账本 dispatched 无接受证据（不重派发——对账/人工裁决）"))
			}
			if row.RequestHash != "" && row.RequestHash != reqHash {
				return nil, restate.ToTerminalError(fmt.Errorf("llm 账本请求 hash 不符（operation 冲突）"))
			}
			if err := deps.Store.MarkLLMCallDispatched(rc, runID, step); err != nil {
				return nil, restate.ToTerminalError(fmt.Errorf("llm 账本 dispatched 失败（零派发）: %w", err))
			}
			r, err := deps.Harness.Call(rc, req)
			// 结果落账（冻结结果引用 + usage——合同表的「同 operation 已有
			// result」的回读源）
			if r != nil {
				unknown := r.Usage.TokensIn == 0 && r.Usage.TokensOut == 0 && !r.Truncated
				resultJSON, _ := json.Marshal(r)
				if lErr := deps.Store.PutLLMCallResult(rc, runID, step, int64(r.Usage.TokensIn), int64(r.Usage.TokensOut), r.Truncated, unknown, "", string(resultJSON)); lErr != nil {
					return nil, restate.ToTerminalError(fmt.Errorf("llm 账本 result 失败（效果未知——不提交结果）: %w", lErr))
				}
			} else if err != nil {
				if lErr := deps.Store.PutLLMCallResult(rc, runID, step, 0, 0, false, true, err.Error(), ""); lErr != nil {
					return nil, restate.ToTerminalError(fmt.Errorf("llm 账本 unknown 失败: %w", lErr))
				}
			}
			return r, err
		}, restate.WithName(StepName("harness", step, "")))
		if res != nil && res.Usage.TokensIn == 0 && res.Usage.TokensOut == 0 && !res.Truncated {
			// 审计 #7：usage 不确定当零——显式 usage_unknown 标注（账本第一块；
			// 完整 prepared/dispatch/result 状态机后置）
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.LLMCall, "harness", "", map[string]any{
				"step": step, "usage_unknown": true,
			})
		}
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
		// 三级熔断 ②：org 日预算每步检查（快照 + 本 run 累计——确定性）→ 冻结
		if d := budgetCheck(ctx, deps.BudgetPolicy, budgetSnap, policy.Accum{Tokens: accTokens, Compute: accCompute}); !d.Allow {
			if err := freezeRun(ctx, deps, in, runID, emit, d.Key); err != nil {
				return RunOutput{}, restate.ToTerminalError(fmt.Errorf("freeze: %w", err))
			}
			// 解冻后重新快照（充值生效；journaled 重放确定性）
			if sess, sErr := deps.Store.GetSession(ctx, in.SessionID); sErr == nil {
				budgetSnap, _ = restate.Run(ctx, func(rc restate.RunContext) (policy.BudgetSnapshot, error) {
					return budgetSnapshotOf(rc, deps.BudgetPolicy, sess.OrgID), nil
				}, restate.WithName("budget-snapshot"))
			}
		}

		if res.ErrCode != "" { // harness 失败终态（error 帧）
			releaseLease()
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "", "", map[string]any{
				"code": res.ErrCode, "message": res.ErrMsg,
			})
			_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
			return RunOutput{}, restate.ToTerminalError(fmt.Errorf("harness: %s: %s", res.ErrCode, res.ErrMsg))
		}

		if len(res.ToolCalls) > 0 {
			// assistant 工具调用消息先入历史：tool 消息必须成对引用
			//（Anthropic 兼容 API 硬校验 'tool_call_id'——真实模型 e2e 实证缺失即 400）
			// 协议形状（id/type/function——与 /runs 协议一致；大写 Go 字段名会被
			// OpenAI 兼容 API 拒识，真实模型 e2e 实证 'tool_call_id' 400）
			rawCalls := make([]json.RawMessage, 0, len(res.ToolCalls))
			protoList := make([]any, 0, len(res.ToolCalls))
			for _, tc := range res.ToolCalls {
				ptc := protocolToolCall(tc)
				b, _ := json.Marshal(ptc)
				rawCalls = append(rawCalls, b)
				protoList = append(protoList, ptc)
			}
			msgs = append(msgs, runs.Message{Role: "assistant", Content: "", ToolCalls: rawCalls, Source: "trusted"})
			// 表写回必须同用协议形状（大写 Go 字段名会被 OpenAI 兼容 API 拒识）
			assistantJSON, _ := json.Marshal(map[string]any{"tool_calls": protoList})
			if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, step, "assistant", assistantJSON); err != nil {
				return RunOutput{}, restate.ToTerminalError(err)
			}
			// 工具分流（worker-架构设计 §1 dispatcher）：代码/命令类 → executor（W2）；
			// 控制类（request_approval）→ W3 awakeable；API 类已在 harness 内联。
			for _, tc := range res.ToolCalls {
				if tc.Name == runs.ToolNextSpeaker {
					groupTurns++
				}
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
					// D 批：批准后解冻（挂起即冻结的对称——dispatch 前恢复算力）
					if cur, cErr := deps.Sessions.GetState(ctx, in.SessionID); cErr == nil && cur.SandboxID != "" {
						_ = deps.Executor.UnfreezeSandbox(ctx, cur.SandboxID)
					}
					// 审计 #7：approved 但已取消 → 不派发（重验取消——此前
					// 直接派发，已取消却先产生新效果的反例关闭）
					if cur, cErr := deps.Sessions.GetState(ctx, in.SessionID); cErr == nil && cur.CancelRequested {
						_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunCanceled, "", "", map[string]any{
							"step": step, "reason": "cancel_after_approval",
						})
						_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunCanceled)
						_ = deps.Sessions.ClearCancel(ctx, in.SessionID)
						return RunOutput{Final: "已取消", Steps: step + 1, Canceled: true}, nil
					}
					if approvalGranted(decision) && deps.ApprovalRouter != nil {
						// 审计 C1：派发前重验（批准后、dispatch 前的撤权窗口——
						// approvers 集合现查，approver 已移除则拒绝）
						var d struct {
							Approver string `json:"approver"`
						}
						_ = json.Unmarshal([]byte(decision), &d)
						if sess, sErr := deps.Store.GetSession(ctx, in.SessionID); sErr == nil {
							if approvers, _, rErr := deps.ApprovalRouter.Route(ctx, policy.ApprovalRequest{
								TenantID: sess.OrgID, Tool: tc.Name, Class: 2, SessionID: in.SessionID, RunID: runID,
							}); rErr == nil && len(approvers) > 0 && d.Approver != "" && !containsStr(approvers, d.Approver) {
								_ = emit.Emit(ctx, in.SessionID, runID, step, event.AuditToolDenied, "deny", tc.Name, map[string]any{
									"step": step, "tool": tc.Name, "risk_class": 2, "reason": "approver_revoked",
								})
								_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
								return RunOutput{}, restate.ToTerminalError(fmt.Errorf("工具 %s 的审批人已撤权（派发前重验）", tc.Name))
							}
						}
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
				slog.Default().Info("runLoop: 工具分流", "run", runID, "step", step, "tool", tc.Name, "id", tc.ID)
				result, computeSeconds, err := dispatchTool(ctx, deps, in, runID, step, cfg, tc, emit)
				if err != nil {
					return RunOutput{}, restate.ToTerminalError(
						fmt.Errorf("dispatch %s (step %d): %w", tc.Name, step, err))
				}
				accCompute += computeSeconds
				if maxCompute > 0 && accCompute > maxCompute {
					return failBudget(step, budgetKeyCompute)
				}
				// tool 消息带 tool_call_id（跨 run 重放时还原配对——Anthropic 硬校验
				// 「No tool output found」：真实模型 e2e 实证的 fake 绿真实红缺口）
				content, _ := json.Marshal(map[string]string{"tool_call_id": tc.ID, "content": result})
				if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, step, "tool", content); err != nil {
					return RunOutput{}, restate.ToTerminalError(err)
				}
				msgs = append(msgs, runs.Message{Role: "tool", Content: result, Source: "sandbox", ToolCallID: tc.ID})
			}
			_ = emit.Emit(ctx, in.SessionID, runID, step, event.StepJournaled, "step", "", nil)
			continue
		}

		if res.Done {
			// 结构化输出契约（正式版架构 期 1）：final 必须符合 cfg.OutputSchema
			if len(cfg.OutputSchema) > 0 {
				if vErr := validateOutputSchema(cfg.OutputSchema, res.Final); vErr != nil {
					_ = emit.Emit(ctx, in.SessionID, runID, step, event.RunFailed, "", "", map[string]any{
						"reason": "output_schema_violation", "message": vErr.Error(),
					})
					_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFailed)
					return RunOutput{}, restate.ToTerminalError(fmt.Errorf("output schema violation: %w", vErr))
				}
			}
			content, _ := json.Marshal(res.Final)
			if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, step, "assistant", content); err != nil {
				return RunOutput{}, restate.ToTerminalError(err)
			}
			// E2+审计 5.2：canonical terminal 先提交（终态+deliverable 同事务）
			// ——提交成功后才发布 completed 事件与释放 claim（事件不得早于
			// 终态提交确认——观察者不得先见未获确认的成功）
			payload, _ := json.Marshal(map[string]any{
				"final": res.Final, "steps": step + 1, "truncated": res.Truncated,
				"tokens_in": accTokens, "compute_seconds": accCompute,
			})
			if err := deps.Store.FinalizeRun(ctx, runID, in.SessionID, sessionapi.RunCompleted, "run_completed", payload); err != nil {
				return RunOutput{}, restate.ToTerminalError(err)
			}
			if eErr := emit.Emit(ctx, in.SessionID, runID, step, event.RunCompleted, "", "", map[string]any{
				"final": res.Final, "steps": step + 1, "truncated": res.Truncated,
			}); eErr != nil {
				// 审计 5.2：事件义务失败不静默（调用者等不到完成的窗口——
				// terminal 由同身份恢复提交补发）
				return RunOutput{}, restate.ToTerminalError(fmt.Errorf("completed 事件写失败（终态已提交——恢复补发）: %w", eErr))
			}
			releaseLease()
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

// requestHashOf 冻结请求的 hash（仲裁合同——同 operation 结果校验）。
func requestHashOf(req *runs.Request) string {
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// buildMessages 组装本轮消息（分层记忆的 W5 前简化形态：system 指令 + 历史 + 本 run 输入）。
func buildMessages(ctx context.Context, st Store, har Harness, in RunInput, runID string, cfg sessionapi.AgentConfig, skills []string) ([]runs.Message, error) {
	history, err := st.ListMessagesForRun(ctx, in.SessionID, runID, 50)
	if err != nil {
		return nil, err
	}
	msgs := []runs.Message{{Role: "system", Content: cfg.Instructions, Source: "trusted"}}
	// 期 6 ①：causal slice 注入——「上次为什么停 + 在等什么」（intent 一等
	// 对象——恢复时模型无需从历史推导为什么停；决策蒸发关闭）
	if waits, wErr := st.ActiveWaits(ctx, in.SessionID); wErr == nil && len(waits) > 0 {
		var sb strings.Builder
		sb.WriteString("当前等待中的意图（上次为什么停）：")
		for _, w := range waits {
			sb.WriteString(fmt.Sprintf(" [%s] 意图: %s；期待条件: %s",
				w["kind"], w["intent"], w["expect"]))
		}
		msgs = append(msgs, runs.Message{Role: "system", Content: sb.String(), Source: "trusted"})
	}
	// 已装技能提示（§11：模型经 read_file 使用 skills/<name>/SKILL.md）
	if len(skills) > 0 {
		msgs = append(msgs, runs.Message{Role: "system", Source: "trusted",
			Content: "【已安装技能】" + strings.Join(skills, "、") +
				"。使用前先 read_file /workspace/skills/<name>/SKILL.md 获取用法说明。"})
	}
	// 共享知识注入（期 3 §D：tenant 级 pgvector 检索——基础数据服务；
	// 查询嵌入经 harness /embed；检索失败静默降级不阻断 run）
	if har != nil {
		if sess, sErr := st.GetSession(ctx, in.SessionID); sErr == nil {
			if emb, eErr := har.Embed(ctx, topicOf(in)); eErr == nil && len(emb) > 0 {
				if items, kErr := st.RetrieveKnowledge(ctx, sess.OrgID, emb, 3); kErr == nil && len(items) > 0 {
					var b strings.Builder
					b.WriteString("【共享知识】")
					for _, it := range items {
						b.WriteString("\n- " + it.Content)
					}
					msgs = append(msgs, runs.Message{Role: "system", Content: b.String(), Source: "trusted"})
				}
			}
		}
	}
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
		// tool 消息：还原 tool_call_id 配对（Anthropic 硬校验；历史旧数据无
		// 该结构时降级为纯文本 tool 消息——研发期清库后不再出现）
		if m.Role == "tool" {
			var tr struct {
				ToolCallID string `json:"tool_call_id"`
				Content    string `json:"content"`
			}
			if err := json.Unmarshal(m.Content, &tr); err == nil && tr.ToolCallID != "" {
				msgs = append(msgs, runs.Message{Role: "tool", Content: tr.Content, ToolCallID: tr.ToolCallID, Source: "trusted"})
				continue
			}
			var text string
			if err := json.Unmarshal(m.Content, &text); err == nil {
				msgs = append(msgs, runs.Message{Role: "tool", Content: text, Source: "trusted"})
			}
			continue
		}
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			msgs = append(msgs, runs.Message{Role: m.Role, Content: text, Source: "trusted"})
			continue
		}
		// assistant 工具调用消息：还原 tool_calls（tool 消息必须成对引用——
		// Anthropic 兼容 API 硬校验；跳过会破坏消息形状）
		var tc struct {
			ToolCalls []json.RawMessage `json:"tool_calls"`
		}
		if err := json.Unmarshal(m.Content, &tc); err == nil && len(tc.ToolCalls) > 0 {
			msgs = append(msgs, runs.Message{Role: m.Role, Content: "", ToolCalls: tc.ToolCalls, Source: "trusted"})
		}
	}
	// 用户输入已写回历史（每 run 一次），此处不再注入——重复注入会随轮次复读
	return msgs, nil
}

// protocolToolCall 把工具调用转成 /runs 协议形状（id/type/function）。
func protocolToolCall(tc ToolCall) struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
} {
	ptc := struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}{ID: tc.ID, Type: "function"}
	ptc.Function.Name = tc.Name
	ptc.Function.Arguments = string(tc.Arguments)
	return ptc
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
