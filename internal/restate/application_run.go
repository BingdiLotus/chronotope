package restate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/contract"
	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/observability"
	restate "github.com/restatedev/sdk-go"
)

// RunError is the stable run terminal error. code/category/retryable are
// machine-readable; message is diagnostic and must not be branched on.
type RunError struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Category  contract.Category `json:"category"`
	Retryable bool              `json:"retryable"`
}

// runErrorFor classifies a terminal run code. Provider failures are only
// retryable when no tool batch has executed yet: retrying a run after an
// application effect could duplicate business actions.
func runErrorFor(code, message string, effectsPossible bool) *RunError {
	def := contract.ClassifyRun(code)
	retryable := def.Retryable
	if def.Category == contract.CategoryProvider && effectsPossible {
		retryable = false
	}
	return &RunError{Code: def.Code, Message: message, Category: def.Category, Retryable: retryable}
}

func providerErrorCode(err error) string {
	if err == nil {
		return "provider_error"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "provider_timeout"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "429"), strings.Contains(text, "rate limit"), strings.Contains(text, "quota"):
		return "provider_rate_limited"
	case strings.Contains(text, "timeout"), strings.Contains(text, "deadline"):
		return "provider_timeout"
	case strings.Contains(text, "status 400"), strings.Contains(text, "status 401"), strings.Contains(text, "status 403"):
		return "provider_invalid_request"
	default:
		return "provider_error"
	}
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
	tenantID := ""
	effectsPossible := false
	started := time.Now()
	finish := func(status sessionapi.RunStatus, step int, final, code, message string, truncated bool) (RunOutput, error) {
		result := RunTerminalResult{Final: final, Usage: usage, Steps: step, Truncated: truncated}
		if code != "" {
			result.Error = runErrorFor(code, message, effectsPossible)
		}
		if err := finalizeTerminal(ctx, deps.Store, in.SessionID, runID, status, result); err != nil {
			return RunOutput{}, err
		}
		errorCode, category, retryable := "", contract.Category(""), false
		if result.Error != nil {
			errorCode, category, retryable = result.Error.Code, result.Error.Category, result.Error.Retryable
		}
		elapsed := time.Since(started)
		observability.RecordRunTerminal(string(status), errorCode, string(category), retryable, elapsed)
		level := slog.LevelInfo
		if status == sessionapi.RunFailed {
			level = slog.LevelWarn
		}
		slog.Default().Log(ctx, level, "run.terminal",
			"run_id", runID, "session_id", in.SessionID, "tenant_id", tenantID,
			"status", status, "error_code", errorCode, "category", category,
			"retryable", retryable, "steps", step, "duration_ms", elapsed.Milliseconds())

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
	binding, err := applicationContext(ctx, deps, in, runID, 0, ToolCall{ID: "binding-check"})
	if err != nil {
		return finish(sessionapi.RunFailed, 0, "", "invalid_application_context", err.Error(), false)
	}
	tenantID = binding.TenantID
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
		providerStarted := time.Now()
		res, callErr := restate.Run(ctx, func(rc restate.RunContext) (*Result, error) { return harnessCallClosure(rc, deps, runID, step, req) }, restate.WithName(StepName("harness", step, "")))
		if callErr != nil {
			code := providerErrorCode(callErr)
			observability.RecordProviderError(code, time.Since(providerStarted))
			slog.Default().Warn("provider.error", "run_id", runID, "tenant_id", tenantID, "step", step, "model", cfg.Model, "error_code", code, "category", contract.CategoryProvider, "duration_ms", time.Since(providerStarted).Milliseconds())
			return finish(sessionapi.RunFailed, step+1, "", code, callErr.Error(), false)
		}
		if res == nil {
			observability.RecordProviderError("provider_error", time.Since(providerStarted))
			return finish(sessionapi.RunFailed, step+1, "", "provider_error", "empty model result", false)
		}
		if res.ErrCode == "" {
			observability.RecordProviderCall("success", time.Since(providerStarted))
		}
		usage.TokensIn += int64(res.Usage.TokensIn)
		usage.TokensOut += int64(res.Usage.TokensOut)
		usage.TokensTotal = usage.TokensIn + usage.TokensOut
		usage.UsageUnknown = usage.UsageUnknown || res.Usage.UsagePartial || (res.Usage.TokensIn == 0 && res.Usage.TokensOut == 0)
		if err := emit.Emit(ctx, in.SessionID, runID, step, event.LLMCall, "llm", "", map[string]any{"step": step, "usage": res.Usage, "usage_unknown": usage.UsageUnknown}); err != nil {
			return finish(sessionapi.RunFailed, step+1, "", "event_error", err.Error(), false)
		}
		if res.ErrCode != "" {
			code := contract.ProviderCode(res.ErrCode)
			observability.RecordProviderError(code, time.Since(providerStarted))
			slog.Default().Warn("provider.error", "run_id", runID, "tenant_id", tenantID, "step", step, "model", cfg.Model, "error_code", code, "category", contract.CategoryProvider, "duration_ms", time.Since(providerStarted).Milliseconds())
			return finish(sessionapi.RunFailed, step+1, "", code, res.ErrMsg, res.Truncated)
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
						stopCode = "application_tool_error"
						if errors.Is(dispatchErr, ErrApplicationResultUnknown) {
							stopCode = "application_tool_result_unknown"
						}
						stopMessage = dispatchErr.Error()
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
		// Any dispatched tool batch may have produced an external effect. From
		// here on, provider failures are not safe to retry automatically.
		effectsPossible = true
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
