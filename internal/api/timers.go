package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/store"
	"github.com/go-chi/chi/v5"
)

type TimerStore interface {
	CreateApplicationTimer(context.Context, timer.Request, string, string) (*timer.Record, bool, error)
	GetApplicationTimer(context.Context, timer.Handle) (*timer.Record, error)
	ListPendingApplicationTimers(context.Context, int) ([]timer.Record, error)
	MarkApplicationTimerSubmitted(context.Context, timer.Handle) error
	RecordApplicationTimerSubmitError(context.Context, timer.Handle, string) error
}

// TimerHandler is mounted behind the same authentication middleware as Handler.
// Targets are operator configuration, not an application/model request field.
type TimerHandler struct {
	Store   TimerStore
	Targets []timer.Target
}

func (h *TimerHandler) RegisterRoutes(r chi.Router) {
	r.Post("/timers", h.create)
	r.Get("/timers/{timerID}", h.get)
}

func (h *TimerHandler) create(w http.ResponseWriter, r *http.Request) {
	var in timer.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, 400, 400, "invalid timer request")
		return
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, 400, 400, "expected one timer request")
		return
	}
	in = in.Normalize()
	if err := in.Validate(); err != nil {
		writeError(w, 400, 400, err.Error())
		return
	}
	if !requireOrg(w, r, in.TenantID) {
		return
	}
	var selected *timer.Target
	for _, target := range h.Targets {
		if !target.Matches(in) {
			continue
		}
		if target.Validate() != nil || selected != nil {
			writeError(w, 503, 503, "invalid or ambiguous configured timer target")
			return
		}
		t := target
		selected = &t
	}
	if selected == nil {
		writeError(w, 403, 403, "timer subject/scope is not configured for tenant")
		return
	}
	if h.Store == nil {
		writeError(w, 503, 503, "timer store unavailable")
		return
	}
	record, _, err := h.Store.CreateApplicationTimer(r.Context(), in, selected.Ref, selected.Digest())
	if err != nil {
		if errors.Is(err, store.ErrTimerConflict) {
			writeError(w, 409, 409, "timer key has a different request")
			return
		}
		writeError(w, 500, 500, "timer admission failed")
		return
	}
	query := url.Values{"tenant_id": {in.TenantID}, "subject_ref": {in.SubjectRef}, "scope_id": {in.ScopeID}, "revision": {strconv.FormatInt(in.Revision, 10)}}
	w.Header().Set("Location", "/timers/"+url.PathEscape(in.TimerID)+"?"+query.Encode())
	writeJSON(w, http.StatusAccepted, record)
}

func (h *TimerHandler) get(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	revision, err := strconv.ParseInt(q.Get("revision"), 10, 64)
	tenantID, subjectRef, scopeID := q.Get("tenant_id"), q.Get("subject_ref"), q.Get("scope_id")
	if err != nil || revision <= 0 || tenantID == "" || subjectRef == "" || scopeID == "" {
		writeError(w, 400, 400, "tenant_id, subject_ref, scope_id and positive revision are required")
		return
	}
	if !requireOrg(w, r, tenantID) {
		return
	}
	if h.Store == nil {
		writeError(w, 503, 503, "timer store unavailable")
		return
	}
	timerID := chi.URLParam(r, "timerID")
	if r.URL.RawPath != "" {
		timerID, err = url.PathUnescape(timerID)
		if err != nil {
			writeError(w, 400, 400, "invalid timer ID")
			return
		}
	}
	row, err := h.Store.GetApplicationTimer(r.Context(), timer.Handle{TenantID: tenantID, TimerID: timerID, Revision: revision})
	if errors.Is(err, store.ErrNotFound) || (err == nil && (row.SubjectRef != subjectRef || row.ScopeID != scopeID)) {
		writeError(w, 404, 404, "timer not found in scope")
		return
	}
	if err != nil {
		writeError(w, 500, 500, "timer query failed")
		return
	}
	writeJSON(w, 200, row)
}

type TimerSubmitter interface {
	Send(context.Context, string, any) error
}

// TimerRecovery only submits durable workflow responsibilities. It never scans
// due_at or performs application actions and is not a second scheduler.
type TimerRecovery struct {
	Store   TimerStore
	Ingress TimerSubmitter
	Logger  Logger
}

func (a *TimerRecovery) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if err := a.Pass(ctx); err != nil && a.Logger != nil {
			a.Logger.Warn("timer submission recovery failed", "err", err)
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (a *TimerRecovery) Pass(ctx context.Context) error {
	if a.Store == nil || a.Ingress == nil {
		return fmt.Errorf("timer recovery requires store and ingress")
	}
	rows, err := a.Store.ListPendingApplicationTimers(ctx, 20)
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = a.Ingress.Send(callCtx, "/"+timer.WorkflowName+"/"+row.WorkflowKey+"/run", row.Handle())
		cancel()
		if err != nil {
			if recordErr := a.Store.RecordApplicationTimerSubmitError(ctx, row.Handle(), "workflow submission not confirmed"); recordErr != nil {
				failures = append(failures, recordErr)
			}
			failures = append(failures, fmt.Errorf("timer %s submission not confirmed: %w", row.WorkflowKey, err))
			continue
		}
		if err := a.Store.MarkApplicationTimerSubmitted(ctx, row.Handle()); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
