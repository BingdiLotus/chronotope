package restate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/observability"
	"github.com/bingdilotus/chronotope/internal/targets"
	restate "github.com/restatedev/sdk-go"
)

type ApplicationTimerStore interface {
	GetApplicationTimer(context.Context, timer.Handle) (*timer.Record, error)
	BeginApplicationTimerCallback(context.Context, timer.Handle) error
	RecordApplicationTimerCallbackError(context.Context, timer.Handle, bool, string, string) error
	ConfirmApplicationTimer(context.Context, timer.Handle) error
}

// TimerTargetResolver resolves the current target by frozen tenant/ref. The
// returned digest is compared with the timer row; a changed digest is blocked.
type TimerTargetResolver interface {
	ResolveTimerTargetByRef(context.Context, string, string) (*timer.Target, string, error)
}

type TimerDeps struct {
	Store      ApplicationTimerStore
	Targets    []timer.Target
	Resolver   TimerTargetResolver
	HTTPClient *http.Client
	// Now is a wall-clock seam for testing a crash before Sleep is journaled.
	// Sleep itself persists an absolute wake-up instant in the Restate journal.
	Now func() time.Time
}

func ApplicationTimerDefinition(deps TimerDeps) restate.ServiceDefinition {
	return restate.NewWorkflow(timer.WorkflowName).Handler("run", restate.NewWorkflowHandler[timer.Handle, timer.Fire](
		func(ctx restate.WorkflowContext, in timer.Handle) (timer.Fire, error) {
			return applicationTimerRun(ctx, deps, in, restate.Key(ctx))
		}))
}

func applicationTimerRun(ctx restate.Context, deps TimerDeps, in timer.Handle, key string) (timer.Fire, error) {
	row, err := restate.Run(ctx, func(rc restate.RunContext) (timer.Record, error) {
		if deps.Store == nil {
			return timer.Record{}, fmt.Errorf("timer store unavailable")
		}
		r, err := deps.Store.GetApplicationTimer(rc, in)
		if err != nil {
			return timer.Record{}, err
		}
		return *r, nil
	}, restate.WithName("timer-load"))
	if err != nil {
		return timer.Fire{}, err
	}
	if key != row.WorkflowKey || key != row.Request.Key() {
		return timer.Fire{}, restate.ToTerminalError(fmt.Errorf("timer workflow key mismatch"))
	}
	if row.State == "acknowledged" {
		return row.Fire(), nil
	}
	// Compute remaining time immediately before creating Sleep. Caching a
	// duration in a preceding Run extends due_at when the worker dies between
	// those two journal entries. sdk-go SysSleep translates this to an absolute
	// timestamp; shared-core matches an existing Sleep by name and preserves its
	// original wake-up timestamp on replay. Always issue Sleep, including zero,
	// so replay consumes the same journal position.
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	delay := row.DueAt.Sub(now())
	if delay < 0 {
		delay = 0
	}
	if err := restate.Sleep(ctx, delay, restate.WithName("timer-until-due")); err != nil {
		return timer.Fire{}, err
	}
	fire := row.Fire()
	_, err = restate.Run(ctx, func(rc restate.RunContext) (bool, error) {
		if err := deliverApplicationTimer(rc, deps, row); err != nil {
			return false, err
		}
		return true, nil
	}, restate.WithName("timer-deliver-fire"), restate.WithInitialRetryInterval(time.Second), restate.WithMaxRetryInterval(time.Minute))
	if err != nil {
		return timer.Fire{}, err
	}
	return fire, nil
}

func deliverApplicationTimer(ctx context.Context, deps TimerDeps, row timer.Record) error {
	started := time.Now()
	var selected *timer.Target
	digest := ""
	if deps.Resolver != nil {
		target, resolvedDigest, err := deps.Resolver.ResolveTimerTargetByRef(ctx, row.TenantID, row.TargetRef)
		if errors.Is(err, targets.ErrAmbiguousTarget) {
			return recordTimerCallbackFailure(ctx, deps, row, true, "timer_configuration_error", "ambiguous configured timer target", started)
		}
		if err != nil || target == nil {
			return recordTimerCallbackFailure(ctx, deps, row, true, "timer_target_unavailable", "configured timer target changed or unavailable", started)
		}
		selected, digest = target, resolvedDigest
	} else {
		for _, target := range deps.Targets {
			if target.Ref != row.TargetRef || !target.Matches(row.Request) {
				continue
			}
			if selected != nil {
				return recordTimerCallbackFailure(ctx, deps, row, true, "timer_configuration_error", "ambiguous configured timer target", started)
			}
			t := target
			selected = &t
		}
		if selected != nil {
			digest = selected.Digest()
		}
	}
	if selected == nil || selected.Validate() != nil || !selected.Matches(row.Request) {
		return recordTimerCallbackFailure(ctx, deps, row, true, "timer_target_unavailable", "configured timer target changed or unavailable", started)
	}
	if digest != row.TargetDigest {
		return recordTimerCallbackFailure(ctx, deps, row, true, "timer_target_changed", "configured timer target changed or unavailable", started)
	}
	// A previous callback may have committed its receipt before the journal entry
	// completed. Checking durable state avoids even an unnecessary repeat call.
	current, err := deps.Store.GetApplicationTimer(ctx, row.Handle())
	if err != nil {
		observability.RecordTimerFire("error", "store_error", time.Since(started))
		return err
	}
	if current.State == "acknowledged" {
		observability.RecordTimerFire("acknowledged", "", time.Since(started))
		return nil
	}
	if err := deps.Store.BeginApplicationTimerCallback(ctx, row.Handle()); err != nil {
		observability.RecordTimerFire("error", "store_error", time.Since(started))
		return err
	}
	payload, err := json.Marshal(row.Fire())
	if err != nil {
		return recordTimerCallbackFailure(ctx, deps, row, true, "timer_configuration_error", "invalid configured timer callback", started)
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, selected.CallbackURL, bytes.NewReader(payload))
	if err != nil {
		return recordTimerCallbackFailure(ctx, deps, row, true, "timer_configuration_error", "invalid configured timer callback", started)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", row.FireID)
	if selected.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+selected.BearerToken)
	}
	client := http.Client{}
	if deps.HTTPClient != nil {
		client = *deps.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return recordTimerCallbackFailure(ctx, deps, row, false, "timer_callback_unknown", "callback response not confirmed", started)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return recordTimerCallbackFailure(ctx, deps, row, false, "timer_callback_rejected", fmt.Sprintf("callback returned HTTP %d", resp.StatusCode), started)
	}
	// 2xx means the application durably admitted fire, not that its action or
	// final external send completed. Failed receipt writes repeat the same fire.
	if err := deps.Store.ConfirmApplicationTimer(ctx, row.Handle()); err != nil {
		return recordTimerCallbackFailure(ctx, deps, row, false, "timer_ack_persist_failed", "callback ACK persistence not confirmed", started)
	}
	elapsed := time.Since(started)
	observability.RecordTimerFire("acknowledged", "", elapsed)
	slog.Default().Info("timer.fire", "tenant_id", row.TenantID, "timer_id", row.TimerID,
		"revision", row.Revision, "fire_id", row.FireID, "target_ref", row.TargetRef,
		"state", "acknowledged", "duration_ms", elapsed.Milliseconds())
	return nil
}

func recordTimerCallbackFailure(ctx context.Context, deps TimerDeps, row timer.Record, blocked bool, code, message string, started time.Time) error {
	if err := deps.Store.RecordApplicationTimerCallbackError(ctx, row.Handle(), blocked, code, message); err != nil {
		return err
	}
	state := "retrying"
	level := slog.LevelWarn
	if blocked {
		state = "blocked"
		level = slog.LevelError
	}
	elapsed := time.Since(started)
	observability.RecordTimerFire(state, code, elapsed)
	slog.Default().Log(ctx, level, "timer.fire", "tenant_id", row.TenantID, "timer_id", row.TimerID,
		"revision", row.Revision, "fire_id", row.FireID, "target_ref", row.TargetRef,
		"state", state, "error_code", code, "blocked", blocked, "duration_ms", elapsed.Milliseconds())
	return fmt.Errorf("timer callback: %s", message)
}
