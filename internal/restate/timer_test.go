package restate

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/targets"
	"github.com/bingdilotus/chronotope/test/contract/fakeapp"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/x/mocks"
	"github.com/stretchr/testify/mock"
)

type timerWorkerStore struct {
	row          timer.Record
	confirmFails int
	confirmed    int
}

type timerRunContext struct{ context.Context }

func (timerRunContext) Log() *slog.Logger         { return slog.Default() }
func (timerRunContext) Request() *restate.Request { return nil }

func (s *timerWorkerStore) GetApplicationTimer(context.Context, timer.Handle) (*timer.Record, error) {
	r := s.row
	return &r, nil
}
func (s *timerWorkerStore) BeginApplicationTimerCallback(context.Context, timer.Handle) error {
	s.row.CallbackAttempts++
	s.row.State = "firing"
	return nil
}
func (s *timerWorkerStore) RecordApplicationTimerCallbackError(_ context.Context, _ timer.Handle, blocked bool, code, message string) error {
	s.row.State = "retrying"
	if blocked {
		s.row.State = "blocked"
	}
	s.row.LastError = message
	s.row.LastErrorCode = code
	return nil
}
func (s *timerWorkerStore) ConfirmApplicationTimer(context.Context, timer.Handle) error {
	if s.confirmFails > 0 {
		s.confirmFails--
		return errors.New("receipt commit failed")
	}
	s.row.State = "acknowledged"
	s.confirmed++
	return nil
}

func timerWorkerScenario(callbackURL string) (*timerWorkerStore, TimerDeps) {
	req := timer.Request{TenantID: "org", SubjectRef: "bot", ScopeID: "channel", TimerID: "reminder", Revision: 1, DueAt: time.Now().Add(time.Hour), OpaqueActionRef: "task:1"}.Normalize()
	target := timer.Target{Ref: "bots", TenantID: req.TenantID, SubjectRef: req.SubjectRef, ScopeID: req.ScopeID, CallbackURL: callbackURL, BearerToken: "callback-token"}
	st := &timerWorkerStore{row: timer.Record{Request: req, WorkflowKey: req.Key(), FireID: req.Fire().FireID, TargetRef: target.Ref, TargetDigest: target.Digest(), State: "pending"}}
	return st, TimerDeps{Store: st, Targets: []timer.Target{target}}
}

func TestTimerCallbackLostResponseAndReceiptFailure(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	requests, logical := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer callback-token" {
			t.Error("missing callback authentication")
		}
		var fire timer.Fire
		if err := json.NewDecoder(r.Body).Decode(&fire); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("Idempotency-Key") != fire.FireID {
			t.Error("unstable callback idempotency identity")
		}
		mu.Lock()
		requests++
		first := requests == 1
		if !seen[fire.FireID] {
			seen[fire.FireID] = true
			logical++
		}
		mu.Unlock()
		if first {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	st, deps := timerWorkerScenario(server.URL)
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err == nil {
		t.Fatal("lost ACK must not be called confirmed")
	}
	if st.row.State != "retrying" || st.row.LastError == "" {
		t.Fatal("unknown callback outcome is not visible")
	}
	st.confirmFails = 1
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err == nil {
		t.Fatal("local receipt commit failure must remain retryable")
	}
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err != nil {
		t.Fatal(err)
	}
	if st.row.State != "acknowledged" || st.confirmed != 1 {
		t.Fatalf("missing ACK: %+v", st)
	}
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 3 || logical != 1 {
		t.Fatalf("requests=%d logical=%d, want 3/1", requests, logical)
	}
}

func TestTimerTargetDriftAndRedirectCannotChangeDestination(t *testing.T) {
	var called atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1); w.WriteHeader(202) }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	st, deps := timerWorkerScenario(redirect.URL)
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err == nil {
		t.Fatal("redirect must not count as durable admission")
	}
	if called.Load() != 0 || st.row.State != "retrying" {
		t.Fatalf("redirect escaped frozen target: %d %s", called.Load(), st.row.State)
	}
	deps.Targets[0].CallbackURL = other.URL
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err == nil {
		t.Fatal("changed target must be blocked")
	}
	if called.Load() != 0 || st.row.State != "blocked" {
		t.Fatalf("changed binding sent callback: %d %s", called.Load(), st.row.State)
	}
}

func TestApplicationTimerJournalReplayKeepsSleepAndFire(t *testing.T) {
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1); w.WriteHeader(202) }))
	defer server.Close()
	st, deps := timerWorkerScenario(server.URL)
	clock := st.row.DueAt.Add(-time.Hour)
	deps.Now = func() time.Time { return clock }
	var journal []any
	firstCtx := mocks.NewMockContext(t)
	execute := func(fn func(restate.RunContext) (any, error), out any, _ ...restate.RunOption) restate.TerminalError {
		v, err := fn(timerRunContext{context.Background()})
		if err != nil {
			return restate.AsTerminalError(err)
		}
		journal = append(journal, v)
		reflect.ValueOf(out).Elem().Set(reflect.ValueOf(v))
		return nil
	}
	firstCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(execute).Once()
	firstCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(execute).Once()
	var slept time.Duration
	firstCtx.EXPECT().Sleep(mock.Anything, mock.Anything).RunAndReturn(func(d time.Duration, _ ...restate.SleepOption) restate.TerminalError { slept = d; return nil }).Once()
	fire, err := applicationTimerRun(restate.WithMockContext(firstCtx), deps, st.row.Handle(), st.row.WorkflowKey)
	if err != nil {
		t.Fatal(err)
	}
	if slept != time.Hour || called.Load() != 1 || len(journal) != 2 {
		t.Fatalf("unexpected execution: sleep=%v calls=%d journal=%d", slept, called.Load(), len(journal))
	}
	// Replay cached entries without running their external side effects. The row
	// cached at admission remains stable; replayed Sleep consumes its original
	// persisted completion, even though the wall clock now suggests zero delay.
	clock = st.row.DueAt.Add(time.Hour)
	index := 0
	secondCtx := mocks.NewMockContext(t)
	replay := func(_ func(restate.RunContext) (any, error), out any, _ ...restate.RunOption) restate.TerminalError {
		reflect.ValueOf(out).Elem().Set(reflect.ValueOf(journal[index]))
		index++
		return nil
	}
	secondCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(replay).Once()
	secondCtx.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(replay).Once()
	secondCtx.EXPECT().Sleep(time.Duration(0), mock.Anything).Return(nil).Once()
	got, err := applicationTimerRun(restate.WithMockContext(secondCtx), deps, st.row.Handle(), st.row.WorkflowKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != fire || called.Load() != 1 || index != len(journal) {
		t.Fatalf("replay changed effect or identity: %+v %+v %d", got, fire, called.Load())
	}
}

func TestApplicationTimerCrashBeforeSleepUsesAbsoluteDeadline(t *testing.T) {
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1); w.WriteHeader(202) }))
	defer server.Close()
	st, deps := timerWorkerScenario(server.URL)
	clock := st.row.DueAt.Add(-time.Hour)
	deps.Now = func() time.Time { return clock }
	var loaded timer.Record
	first := mocks.NewMockContext(t)
	first.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(fn func(restate.RunContext) (any, error), out any, _ ...restate.RunOption) restate.TerminalError {
			v, err := fn(timerRunContext{context.Background()})
			if err != nil {
				return restate.AsTerminalError(err)
			}
			loaded = v.(timer.Record)
			reflect.ValueOf(out).Elem().Set(reflect.ValueOf(v))
			return nil
		}).Once()
	first.EXPECT().Sleep(time.Hour, mock.Anything).RunAndReturn(func(time.Duration, ...restate.SleepOption) restate.TerminalError {
		panic("crash before Sleep journal write")
	}).Once()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected simulated crash")
			}
		}()
		_, _ = applicationTimerRun(restate.WithMockContext(first), deps, st.row.Handle(), st.row.WorkflowKey)
	}()
	if called.Load() != 0 {
		t.Fatal("callback occurred before durable wait")
	}
	// Only timer-load has a journal result. Recover after the original due_at:
	// it must create an immediate Sleep, not another hour from the old duration.
	clock = st.row.DueAt.Add(2 * time.Hour)
	second := mocks.NewMockContext(t)
	second.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ func(restate.RunContext) (any, error), out any, _ ...restate.RunOption) restate.TerminalError {
			reflect.ValueOf(out).Elem().Set(reflect.ValueOf(loaded))
			return nil
		}).Once()
	second.EXPECT().Sleep(time.Duration(0), mock.Anything).Return(nil).Once()
	second.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(fn func(restate.RunContext) (any, error), out any, _ ...restate.RunOption) restate.TerminalError {
			v, err := fn(timerRunContext{context.Background()})
			if err != nil {
				return restate.AsTerminalError(err)
			}
			reflect.ValueOf(out).Elem().Set(reflect.ValueOf(v))
			return nil
		}).Once()
	if _, err := applicationTimerRun(restate.WithMockContext(second), deps, st.row.Handle(), st.row.WorkflowKey); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 1 {
		t.Fatalf("recovered callback count %d", called.Load())
	}
}

type dynamicTimerResolver struct {
	target *timer.Target
	digest string
	err    error
}

func (r dynamicTimerResolver) ResolveTimerTargetByRef(context.Context, string, string) (*timer.Target, string, error) {
	return r.target, r.digest, r.err
}

func TestDynamicTimerTargetUpdateBlocksOldTimer(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	st, deps := timerWorkerScenario(server.URL)
	oldDigest := st.row.TargetDigest
	moved := timer.Target{Ref: st.row.TargetRef, TenantID: st.row.TenantID, SubjectRef: st.row.SubjectRef,
		ScopeID: st.row.ScopeID, CallbackURL: "http://moved.example/fire", BearerToken: "callback-token"}
	deps.Targets = nil
	deps.Resolver = dynamicTimerResolver{target: &moved, digest: moved.Digest()}
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err == nil {
		t.Fatal("changed dynamic target must block the old timer")
	}
	if calls.Load() != 0 || st.row.State != "blocked" || st.row.LastErrorCode != "timer_target_changed" {
		t.Fatalf("old timer was silently redirected: calls=%d row=%+v", calls.Load(), st.row)
	}
	if moved.Digest() == oldDigest {
		t.Fatal("test setup must change the target digest")
	}
	// Delete/revocation is also explicit and blocked.
	deps.Resolver = dynamicTimerResolver{err: targets.ErrTimerTargetUnavailable}
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err == nil {
		t.Fatal("revoked target must remain blocked")
	}
	if calls.Load() != 0 || st.row.LastErrorCode != "timer_target_unavailable" {
		t.Fatalf("revoked target outcome: calls=%d row=%+v", calls.Load(), st.row)
	}
}

func TestDynamicTimerTargetMatchingDigestDeliversFire(t *testing.T) {
	receiver := fakeapp.New("callback-token")
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	st, deps := timerWorkerScenario(server.URL + "/timer-fires")
	target := deps.Targets[0]
	deps.Targets = nil
	deps.Resolver = dynamicTimerResolver{target: &target, digest: st.row.TargetDigest}
	if err := deliverApplicationTimer(context.Background(), deps, st.row); err != nil {
		t.Fatal(err)
	}
	fires := receiver.Fires()
	if len(fires) != 1 {
		t.Fatalf("fire not delivered: %+v", fires)
	}
	fire := fires[0]
	if fire.Payload != st.row.Fire() || fire.IdempotencyKey != st.row.FireID || fire.Authorization != "Bearer callback-token" {
		t.Fatalf("timer callback contract drift: %+v", fire)
	}
	if st.row.State != "acknowledged" || st.row.LastErrorCode != "" {
		t.Fatalf("timer ACK state: %+v", st.row)
	}
}
