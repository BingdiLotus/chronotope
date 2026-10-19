package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/store"
)

func TestApplicationTimerAdmissionAndRecovery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	org := "org_timer_" + randSuffix()
	if err := s.CreateOrg(ctx, org, "timer tests"); err != nil {
		t.Fatal(err)
	}
	req := timer.Request{TenantID: org, SubjectRef: "bot", ScopeID: "channel", TimerID: "reminder", Revision: 1, DueAt: time.Now().Add(time.Hour), OpaqueActionRef: "task:1"}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	created := make(chan bool, 8)
	for range 8 {
		wg.Go(func() {
			row, fresh, err := s.CreateApplicationTimer(ctx, req, "bots", "digest")
			if err == nil && (row.WorkflowKey != req.Key() || row.FireID != req.Fire().FireID) {
				err = errors.New("unstable timer identity")
			}
			errs <- err
			created <- fresh
		})
	}
	wg.Wait()
	close(errs)
	close(created)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for fresh := range created {
		if fresh {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("created %d timers, want one", n)
	}
	changed := req
	changed.OpaqueActionRef = "other"
	if _, _, err := s.CreateApplicationTimer(ctx, changed, "bots", "digest"); !errors.Is(err, store.ErrTimerConflict) {
		t.Fatalf("want payload conflict, got %v", err)
	}
	changed = req
	changed.ScopeID = "other"
	if _, _, err := s.CreateApplicationTimer(ctx, changed, "bots", "digest"); !errors.Is(err, store.ErrTimerConflict) {
		t.Fatalf("scope mutation must conflict, got %v", err)
	}
	rows, err := s.ListPendingApplicationTimers(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.WorkflowKey == req.Key() {
			found = true
		}
	}
	if !found {
		t.Fatal("durable submission duty missing")
	}
	if err := s.BeginApplicationTimerCallback(ctx, req.Handle()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordApplicationTimerCallbackError(ctx, req.Handle(), false, "response lost"); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetApplicationTimer(ctx, req.Handle())
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "retrying" || row.CallbackAttempts != 1 || row.LastError != "response lost" {
		t.Fatalf("callback failure missing: %+v", row)
	}
	if err := s.ConfirmApplicationTimer(ctx, req.Handle()); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkApplicationTimerSubmitted(ctx, req.Handle()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordApplicationTimerCallbackError(ctx, req.Handle(), true, "late error"); err != nil {
		t.Fatal(err)
	}
	row, err = s.GetApplicationTimer(ctx, req.Handle())
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "acknowledged" || row.AcknowledgedAt == nil || row.LastError != "" {
		t.Fatalf("late submission/error overwrote ACK: %+v", row)
	}
	unknown := req.Handle()
	unknown.TenantID = "other"
	if _, err := s.GetApplicationTimer(ctx, unknown); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant timer visible: %v", err)
	}
	if err := s.ConfirmApplicationTimer(ctx, unknown); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant update succeeded: %v", err)
	}
	changed = req
	changed.Revision++
	if _, fresh, err := s.CreateApplicationTimer(ctx, changed, "bots", "digest"); err != nil || !fresh {
		t.Fatalf("new revision admission: %v %v", fresh, err)
	}
}
