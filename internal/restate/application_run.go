package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	restate "github.com/restatedev/sdk-go"
)

type RunError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RunUsage struct {
	TokensIn       int64   `json:"tokens_in"`
	TokensOut      int64   `json:"tokens_out"`
	TokensTotal    int64   `json:"tokens_total"`
	ComputeSeconds float64 `json:"compute_seconds"`
	UsageUnknown   bool    `json:"usage_unknown"`
}

type RunTerminalResult struct {
	Final     string    `json:"final"`
	Error     *RunError `json:"error"`
	Usage     RunUsage  `json:"usage"`
	Steps     int       `json:"steps"`
	Truncated bool      `json:"truncated,omitempty"`
}

// The application profile uses the same durable workflow/history owner while
// exposing only its fixed registry. It cannot enter legacy SDK/MCP/sandbox loops.
func runApplicationLoop(ctx restate.Context, deps *Deps, in RunInput, runID string, cfg sessionapi.AgentConfig) (RunOutput, error) {
	emit := &Emitter{Store: deps.Store}
	usage := RunUsage{}
	finish := func(status sessionapi.RunStatus, step int, final, code, message string, truncated bool) (RunOutput, error) {
		result := RunTerminalResult{Final: final, Usage: usage, Steps: step, Truncated: truncated}
		if code != "" {
			result.Error = &RunError{Code: code, Message: message}
		}
		if err := finalizeTerminal(ctx, deps.Store, in.SessionID, runID, status, result); err != nil {
			return RunOutput{}, err
		}
		typ := event.RunCompleted
		if status == sessionapi.RunFailed {
			typ = event.RunFailed
		}
		if status == sessionapi.RunCanceled {
			typ = event.RunCanceled
		}
		if err := emit.Emit(ctx, in.SessionID, runID, step, typ, "", "", map[string]any{"final": result.Final, "error": result.Error, "usage": result.Usage, "steps": result.Steps, "truncated": result.Truncated}); err != nil {
			return RunOutput{}, restate.ToTerminalError(err)
		}
		if status == sessionapi.RunFailed {
			return RunOutput{}, restate.ToTerminalError(fmt.Errorf("%s: %s", code, message))
		}
		return RunOutput{Final: final, Steps: step, Canceled: status == sessionapi.RunCanceled}, nil
	}
	if err := cfg.ValidateApplication(); err != nil {
		return finish(sessionapi.RunFailed, 0, "", "invalid_application_config", err.Error(), false)
	}
	if _, err := applicationContext(ctx, deps, in, runID, 0, ToolCall{ID: "binding-check"}); err != nil {
		return finish(sessionapi.RunFailed, 0, "", "invalid_application_context", err.Error(), false)
	}
	deadline, err := runDeadline(ctx, deps, runID)
	if err != nil || deadline.IsZero() {
		return finish(sessionapi.RunFailed, 0, "", "invalid_run_deadline", "application run requires a bound absolute deadline", false)
	}
	if err := emit.Emit(ctx, in.SessionID, runID, 0, event.RunStarted, "", "", map[string]any{"input": in.Input, "model": cfg.Model}); err != nil {
		return finish(sessionapi.RunFailed, 0, "", "event_error", err.Error(), false)
	}
	if err := deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunRunning); err != nil {
		return finish(sessionapi.RunFailed, 0, "", "store_error", err.Error(), false)
	}
	if err := verifyCredential(ctx, deps, runID); err != nil {
		return finish(sessionapi.RunFailed, 0, "", "credential_error", err.Error(), false)
	}
	if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, 0, "user", json.RawMessage(mustJSONString(in.Input))); err != nil {
		return finish(sessionapi.RunFailed, 0, "", "history_error", err.Error(), false)
	}
	messages, err := restate.Run(ctx, func(rc restate.RunContext) ([]runs.Message, error) {
		return buildMessages(rc, deps.Store, nil, in, runID, cfg, nil)
	}, restate.WithName("application-build-messages"))
	if err != nil {
		return finish(sessionapi.RunFailed, 0, "", "history_error", err.Error(), false)
	}
	stepLimit, outputTokens, _ := executionLimits(cfg)
	lastFingerprint, streak := "", 0
	for step := 0; step < stepLimit; step++ {
		current, stateErr := deps.Sessions.GetState(ctx, in.SessionID)
		if stateErr != nil {
			return finish(sessionapi.RunFailed, step, "", "session_error", stateErr.Error(), false)
		}
		if current.CancelRequested {
			_ = deps.Sessions.ClearCancel(ctx, in.SessionID)
			return finish(sessionapi.RunCanceled, step, "已取消", "canceled", "run canceled", false)
		}
		if current.Phase == sessionapi.PhasePaused {
			return finish(sessionapi.RunFailed, step, "", "session_paused", "session is paused", false)
		}
		remaining, err := remainingTime(ctx, deadline, step)
		if err != nil {
			return finish(sessionapi.RunFailed, step, "", "deadline_error", err.Error(), false)
		}
		if remaining <= 0 {
			return finish(sessionapi.RunFailed, step, "", "deadline_exceeded", "run deadline exceeded", false)
		}
		req := &runs.Request{Protocol: runs.ProtocolVersion, RunID: runID, SessionID: in.SessionID, Step: step, Model: cfg.Model, Messages: messages, Tools: applicationToolsForModel(cfg), MaxTurns: 1, MaxOutputBytes: 524288, MaxOutputTokens: outputTokens, TimeoutMS: remaining}
		res, callErr := restate.Run(ctx, func(rc restate.RunContext) (*Result, error) { return harnessCallClosure(rc, deps, runID, step, req) }, restate.WithName(StepName("harness", step, "")))
		if callErr != nil {
			return finish(sessionapi.RunFailed, step+1, "", "harness_error", callErr.Error(), false)
		}
		if res == nil {
			return finish(sessionapi.RunFailed, step+1, "", "harness_error", "empty model result", false)
		}
		usage.TokensIn += int64(res.Usage.TokensIn)
		usage.TokensOut += int64(res.Usage.TokensOut)
		usage.TokensTotal = usage.TokensIn + usage.TokensOut
		usage.UsageUnknown = usage.UsageUnknown || res.Usage.UsagePartial || (res.Usage.TokensIn == 0 && res.Usage.TokensOut == 0)
		if err := emit.Emit(ctx, in.SessionID, runID, step, event.LLMCall, "llm", "", map[string]any{"step": step, "usage": res.Usage, "usage_unknown": usage.UsageUnknown}); err != nil {
			return finish(sessionapi.RunFailed, step+1, "", "event_error", err.Error(), false)
		}
		if res.ErrCode != "" {
			return finish(sessionapi.RunFailed, step+1, "", res.ErrCode, res.ErrMsg, res.Truncated)
		}
		if res.Truncated {
			return finish(sessionapi.RunFailed, step+1, "", "model_output_truncated", "model output limit reached; no partial tools executed", true)
		}
		if len(res.ToolCalls) == 0 {
			if !res.Done {
				return finish(sessionapi.RunFailed, step+1, "", "harness_incomplete", "missing terminal model result", false)
			}
			if len(cfg.OutputSchema) > 0 {
				if err := validateOutputSchema(cfg.OutputSchema, res.Final); err != nil {
					return finish(sessionapi.RunFailed, step+1, "", "output_schema_violation", err.Error(), false)
				}
			}
			if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, step, "assistant", json.RawMessage(mustJSONString(res.Final))); err != nil {
				return finish(sessionapi.RunFailed, step+1, "", "history_error", err.Error(), false)
			}
			return finish(sessionapi.RunCompleted, step+1, res.Final, "", "", false)
		}
		rawCalls := make([]json.RawMessage, 0, len(res.ToolCalls))
		for _, tc := range res.ToolCalls {
			encoded, _ := json.Marshal(protocolToolCall(tc))
			rawCalls = append(rawCalls, encoded)
		}
		assistantText := strings.Join(res.Deltas, "")
		messages = append(messages, runs.Message{Role: "assistant", Content: assistantText, ToolCalls: rawCalls, Source: "trusted"})
		assistantJSON, _ := json.Marshal(map[string]any{"content": assistantText, "tool_calls": rawCalls})
		if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, step, "assistant", assistantJSON); err != nil {
			return finish(sessionapi.RunFailed, step+1, "", "history_error", err.Error(), false)
		}
		stopCode, stopMessage := "", ""
		for _, tc := range res.ToolCalls {
			result := ""
			if stopCode != "" {
				result = toolErrorResult("skipped", stopMessage)
			} else {
				// Every model call is paired even when rejected or not dispatched.
				current, stateErr = deps.Sessions.GetState(ctx, in.SessionID)
				if stateErr != nil || current.CancelRequested || current.Phase == sessionapi.PhasePaused {
					stopCode, stopMessage = "dispatch_stopped", "session stopped before tool dispatch"
				}
				fp := toolFingerprint(tc.Name, tc.Arguments)
				if fp == lastFingerprint {
					streak++
				} else {
					lastFingerprint, streak = fp, 1
				}
				if streak >= noProgressStreak {
					stopCode, stopMessage = "no_progress", "repeated tool arguments without progress"
				}
				remaining, err = remainingTime(ctx, deadline, step)
				if err != nil || remaining <= 0 {
					stopCode, stopMessage = "deadline_exceeded", "run deadline exceeded before tool dispatch"
				}
				if stopCode != "" {
					result = toolErrorResult("skipped", stopMessage)
				} else {
					_ = emit.Emit(ctx, in.SessionID, runID, step, event.ToolCall, "tool", tc.ID, map[string]any{"id": tc.ID, "name": tc.Name, "arguments": tc.Arguments})
					var dispatchErr error
					result, _, dispatchErr = dispatchApplicationTool(ctx, deps, in, runID, step, cfg, tc)
					if dispatchErr != nil {
						stopCode, stopMessage = "application_tool_error", dispatchErr.Error()
						if result == "" {
							result = toolErrorResult(stopCode, stopMessage)
						}
					}
				}
			}
			content, _ := json.Marshal(map[string]string{"tool_call_id": tc.ID, "content": result, "source": "untrusted"})
			if err := deps.Store.AppendMessage(ctx, in.SessionID, runID, step, "tool", content); err != nil {
				return finish(sessionapi.RunFailed, step+1, "", "history_error", err.Error(), false)
			}
			messages = append(messages, runs.Message{Role: "tool", Content: result, ToolCallID: tc.ID, Source: "untrusted"})
		}
		if stopCode != "" {
			return finish(sessionapi.RunFailed, step+1, "", stopCode, stopMessage, false)
		}
		_ = emit.Emit(ctx, in.SessionID, runID, step, event.StepJournaled, "step", "", nil)
	}
	return finish(sessionapi.RunFailed, stepLimit, "", "max_steps", "model step limit reached", false)
}

func finalizeTerminal(ctx context.Context, st Store, sessionID, runID string, status sessionapi.RunStatus, result RunTerminalResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	kind := "run_" + string(status)
	if durable, ok := ctx.(restate.Context); ok {
		_, err = restate.Run(durable, func(rc restate.RunContext) (struct{}, error) {
			return struct{}{}, st.FinalizeRun(rc, runID, sessionID, status, kind, payload)
		}, restate.WithName("finalize-"+string(status)))
		return err
	}
	return st.FinalizeRun(ctx, runID, sessionID, status, kind, payload)
}

func messageSource(source string) string {
	if source == "untrusted" {
		return source
	}
	return "trusted"
}
