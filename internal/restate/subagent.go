package restate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// dispatchSubagent 子 Agent 派发（worker-架构设计 §2 子 Agent 分支；可组合主张）：
// 建子会话 + 子 run（journaled，确定性 id，重放不重建）→ child run_workflow
// （durable 等待子任务完成，挂起零进程占用）→ subagent.spawned/completed 事件 →
// 结果回喂父 agent。子任务失败不致命：错误作为 tool_result 回喂，父 agent 可决策。
func dispatchSubagent(ctx restate.Context, deps *Deps, in RunInput, runID string, step int, cfg sessionapi.AgentConfig, tc ToolCall, emit *Emitter) (string, float64, error) {
	args := parseToolArguments(tc)
	agentID, _ := args["agent"].(string)
	input, _ := args["input"].(string)
	if agentID == "" || input == "" {
		return "", 0, restate.ToTerminalError(fmt.Errorf("spawn_subagent 需要 agent 与 input"))
	}
	seed := runID + ":" + strconv.Itoa(step) + ":" + tc.ID
	childRunID := "r_sub_" + hashSeed(seed)
	childSessionID := "s_sub_" + hashSeed(seed)

	// spawnOutcome 是 journaled 的建行结果（字段导出：重放回放同一 id）。
	type spawnOutcome struct {
		SessionID string `json:"session_id"`
		RunID     string `json:"run_id"`
		Agent     string `json:"agent"`
	}
	outcome, err := restate.Run(ctx, func(rc restate.RunContext) (*spawnOutcome, error) {
		parent, err := deps.Store.GetSession(rc, in.SessionID)
		if err != nil {
			return nil, err
		}
		agent, err := deps.Store.GetAgent(rc, agentID)
		if err != nil {
			return nil, err
		}
		if err := deps.Store.CreateSession(rc, childSessionID, parent.OrgID, agentID); err != nil {
			return nil, err
		}
		if _, err := deps.Store.CreateRun(rc, childRunID, childSessionID,
			json.RawMessage(`{"trigger":"subagent","parent_run_id":"`+runID+`"}`),
			map[string]any{"model": agent.Config.Model, "agent_id": agentID}); err != nil {
			return nil, err
		}
		return &spawnOutcome{SessionID: childSessionID, RunID: childRunID, Agent: agentID}, nil
	}, restate.WithName(StepName("subagent", step, tc.ID)))
	if err != nil {
		return "", 0, err
	}
	// 子会话对象状态：对象调用在 Run 闭包外（SDK 禁闭包内 Context 操作）
	agentRow, aErr := deps.Store.GetAgent(ctx, agentID)
	if aErr != nil {
		return "", 0, aErr
	}
	if err := deps.Sessions.Create(ctx, childSessionID, agentRow.Config); err != nil {
		return "", 0, err
	}

	_ = emit.Emit(ctx, in.SessionID, runID, step, event.SubagentSpawned, "subagent", tc.Name, map[string]any{
		"step": step, "child_session_id": outcome.SessionID, "child_run_id": outcome.RunID,
		"agent": outcome.Agent, "input": input,
	})

	// child run_workflow：durable 等待（挂起零进程占用；重放回放同一结果）
	childOut, childErr := restate.Workflow[RunOutput](ctx, RunWorkflowName, outcome.RunID, "run").
		Request(RunInput{SessionID: outcome.SessionID, Input: input})

	if childErr != nil {
		// 子任务失败不致命：错误回喂父 agent（其可决策重试/换策略）
		// kind 用空 → 以事件类型为 dedupe 键（与 spawned 区分，否则被幂等吞掉）
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.SubagentCompleted, "", "", map[string]any{
			"child_run_id": outcome.RunID, "error": childErr.Error(),
		})
		return fmt.Sprintf(`{"name":%q,"result":{"child_run_id":%q,"child_session_id":%q,"error":%q}}`,
			tc.Name, outcome.RunID, outcome.SessionID, childErr.Error()), 0, nil
	}
	_ = emit.Emit(ctx, in.SessionID, runID, step, event.SubagentCompleted, "", "", map[string]any{
		"child_run_id": outcome.RunID, "final": childOut.Final, "steps": childOut.Steps,
	})
	return fmt.Sprintf(`{"name":%q,"result":{"child_run_id":%q,"child_session_id":%q,"final":%s}}`,
		tc.Name, outcome.RunID, outcome.SessionID, mustJSONString(childOut.Final)), 0, nil
}

// hashSeed 确定性 id 后缀（重放重放同一 id——子会话/子 run 幂等键）。
func hashSeed(seed string) string {
	h := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(h[:])[:12]
}

var _ = runs.ToolSpawnSubagent // 词汇引用稳定
var _ = sessionapi.AgentConfig{}
