package restate

import (
	"encoding/json"
	"fmt"
	"strconv"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
)

// groupTools 群聊主持工具（moderator 决策「下一位谁发言」，发言决策 journaled）。
func groupTools() []runs.Tool {
	return []runs.Tool{
		{Type: "function", Name: runs.ToolNextSpeaker, RiskClass: 1},
	}
}

// speakAsParticipant 成员发言（落地方案 §14：成员 child run，durable await；
// 底层与子 Agent 同用 child run 管道）。args: {participant: index, instruction}。
func speakAsParticipant(ctx restate.Context, deps *Deps, in RunInput, runID string, step int, state SessionState, tc ToolCall, emit *Emitter) (string, float64, error) {
	args := parseToolArguments(tc)
	idx, ok := args["participant"].(float64)
	instruction, _ := args["instruction"].(string)
	if !ok || int(idx) < 0 || int(idx) >= len(state.Participants) {
		return "", 0, restate.ToTerminalError(fmt.Errorf("next_speaker: participant 越界（%v/%d）", idx, len(state.Participants)))
	}
	p := state.Participants[int(idx)]
	seed := runID + ":" + strconv.Itoa(step) + ":" + tc.ID
	childRunID := "r_turn_" + hashSeed(seed)
	childSessionID := "s_turn_" + hashSeed(seed)

	type turnOutcome struct {
		SessionID string `json:"session_id"`
		RunID     string `json:"run_id"`
	}
	outcome, err := restate.Run(ctx, func(rc restate.RunContext) (*turnOutcome, error) {
		parent, err := deps.Store.GetSession(rc, in.SessionID)
		if err != nil {
			return nil, err
		}
		if err := deps.Store.CreateSession(rc, childSessionID, parent.OrgID, p.AgentID); err != nil {
			return nil, err
		}
		if _, err := deps.Store.CreateRun(rc, childRunID, childSessionID,
			json.RawMessage(`{"trigger":"group_turn","parent_run_id":"`+runID+`"}`),
			map[string]any{"agent_id": p.AgentID, "role": p.Role}); err != nil {
			return nil, err
		}
		return &turnOutcome{SessionID: childSessionID, RunID: childRunID}, nil
	}, restate.WithName(StepName("group-turn", step, tc.ID)))
	if err != nil {
		return "", 0, err
	}
	member, mErr := deps.Store.GetAgent(ctx, p.AgentID)
	if mErr != nil {
		return "", 0, mErr
	}
	if err := deps.Sessions.Create(ctx, outcome.SessionID, member.Config); err != nil {
		return "", 0, err
	}
	_ = emit.Emit(ctx, in.SessionID, runID, step, event.GroupTurn, "group", tc.Name, map[string]any{
		"step": step, "participant": int(idx), "agent_id": p.AgentID, "role": p.Role,
		"child_run_id": outcome.RunID, "instruction": instruction,
	})
	// 成员发言 = child run（成员 config + 主持人指令；黑板上下文 = 共享消息日志）
	childOut, childErr := restate.Workflow[RunOutput](ctx, RunWorkflowName, outcome.RunID, "run").
		Request(RunInput{SessionID: outcome.SessionID, Input: instruction})
	if childErr != nil {
		return fmt.Sprintf(`{"name":%q,"result":{"participant":%d,"agent_id":%q,"error":%q}}`,
			tc.Name, int(idx), p.AgentID, childErr.Error()), 0, nil
	}
	return fmt.Sprintf(`{"name":%q,"result":{"participant":%d,"agent_id":%q,"final":%s}}`,
		tc.Name, int(idx), p.AgentID, mustJSONString(childOut.Final)), 0, nil
}
