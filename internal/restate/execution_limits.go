package restate

import (
	"context"
	"fmt"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	restate "github.com/restatedev/sdk-go"
)

func executionLimits(cfg sessionapi.AgentConfig) (steps, outputTokens int, duration time.Duration) {
	steps, outputTokens, duration = cfg.MaxSteps, cfg.MaxOutputTokens, time.Duration(cfg.MaxDurationMS)*time.Millisecond
	if steps <= 0 {
		steps = sessionapi.DefaultMaxSteps
	}
	if outputTokens <= 0 {
		outputTokens = sessionapi.DefaultMaxOutputTokens
	}
	if duration <= 0 {
		duration = time.Duration(sessionapi.DefaultMaxDurationMS) * time.Millisecond
	}
	return
}

func runDeadline(ctx context.Context, deps *Deps, runID string) (time.Time, error) {
	run, err := deps.Store.GetRun(ctx, runID)
	if err != nil || run == nil {
		return time.Time{}, fmt.Errorf("run deadline binding unavailable")
	}
	value, _ := run.Bound["deadline_at"].(string)
	if value == "" {
		return time.Time{}, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid bound run deadline")
	}
	return deadline, nil
}

func boundedCallContext(ctx context.Context, deps *Deps, runID string) (context.Context, context.CancelFunc, error) {
	deadline, err := runDeadline(ctx, deps, runID)
	if err != nil {
		return nil, nil, err
	}
	if deadline.IsZero() {
		return ctx, func() {}, nil
	}
	if !time.Now().Before(deadline) {
		return nil, nil, fmt.Errorf("run deadline exceeded")
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	return callCtx, cancel, nil
}

// Time is journaled before constructing a model request. Its timeout and hash
// therefore remain stable when the same step is replayed.
func remainingTime(ctx restate.Context, deadline time.Time, step int) (int64, error) {
	return restate.Run(ctx, func(restate.RunContext) (int64, error) {
		remaining := time.Until(deadline).Milliseconds()
		if remaining <= 0 {
			return 0, nil
		}
		return remaining, nil
	}, restate.WithName(StepName("remaining-time", step, "")))
}
