package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/store"
	"github.com/go-chi/chi/v5"
)

type timerAPIStore struct {
	rows       map[string]*timer.Record
	failCreate bool
	submitted  int
}

func (s *timerAPIStore) CreateApplicationTimer(_ context.Context, in timer.Request, ref, digest string) (*timer.Record, bool, error) {
	if s.failCreate {
		return nil, false, errors.New("database unavailable")
	}
	if s.rows == nil {
		s.rows = map[string]*timer.Record{}
	}
	if row := s.rows[in.Key()]; row != nil {
		if row.RequestDigest != in.Digest() {
			return nil, false, store.ErrTimerConflict
		}
		return row, false, nil
	}
	r := &timer.Record{Request: in, WorkflowKey: in.Key(), FireID: in.Fire().FireID, RequestDigest: in.Digest(), TargetRef: ref, TargetDigest: digest, State: "pending"}
	s.rows[in.Key()] = r
	return r, true, nil
}
func (s *timerAPIStore) GetApplicationTimer(_ context.Context, h timer.Handle) (*timer.Record, error) {
	key := (timer.Request{TenantID: h.TenantID, TimerID: h.TimerID, Revision: h.Revision}).Key()
	r := s.rows[key]
	if r == nil {
		return nil, store.ErrNotFound
	}
	return r, nil
}
func (s *timerAPIStore) ListPendingApplicationTimers(context.Context, int) ([]timer.Record, error) {
	var out []timer.Record
	for _, r := range s.rows {
		if r.SubmittedAt == nil && r.State != "acknowledged" {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (s *timerAPIStore) MarkApplicationTimerSubmitted(ctx context.Context, h timer.Handle) error {
	r, err := s.GetApplicationTimer(ctx, h)
	if err != nil {
		return err
	}
	now := time.Now()
	r.SubmittedAt = &now
	r.State = "scheduled"
	s.submitted++
	return nil
}
func (s *timerAPIStore) RecordApplicationTimerSubmitError(ctx context.Context, h timer.Handle, code, message string) error {
	r, err := s.GetApplicationTimer(ctx, h)
	if err != nil {
		return err
	}
	r.LastError = message
	r.LastErrorCode = code
	r.SubmitAttempts++
	return nil
}

func timerAPIScenario(t *testing.T) (*timerAPIStore, http.Handler, timer.Request) {
	t.Helper()
	st := &timerAPIStore{}
	base := newFakeStore()
	base.apiKeys = []*store.APIKeyRow{{OrgID: "org-a", KeyHash: sha256Hex("key-a")}, {OrgID: "org-b", KeyHash: sha256Hex("key-b")}}
	h := &TimerHandler{Store: st, Targets: []timer.Target{{Ref: "bots", TenantID: "org-a", SubjectRef: "bot", ScopeID: "channel", CallbackURL: "http://bots/fire"}}}
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	auth := New(base, nil, nil).AuthMiddleware("on", "")(r)
	return st, auth, timer.Request{TenantID: "org-a", SubjectRef: "bot", ScopeID: "channel", TimerID: "reminder", Revision: 1, DueAt: time.Now().Add(time.Hour), OpaqueActionRef: "task:1"}
}

func timerAPIRequest(router http.Handler, method, path, key string, payload any) *httptest.ResponseRecorder {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func TestApplicationTimerAPIAdmissionAndScope(t *testing.T) {
	st, router, in := timerAPIScenario(t)
	for _, key := range []string{"", "key-b"} {
		w := timerAPIRequest(router, "POST", "/timers", key, in)
		want := 401
		if key != "" {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("auth %q: %d %s", key, w.Code, w.Body)
		}
		body := responseObject(t, w)
		if key == "" && body["error_code"] != "unauthorized" {
			t.Fatalf("auth error code: %v", body)
		}
		if key == "key-b" && body["error_code"] != "tenant_mismatch" {
			t.Fatalf("tenant error code: %v", body)
		}
	}
	if len(st.rows) != 0 {
		t.Fatal("unauthorized request persisted a timer")
	}
	w := timerAPIRequest(router, "POST", "/timers", "key-a", in)
	if w.Code != 202 {
		t.Fatalf("admit: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "callback_url") || strings.Contains(w.Body.String(), "Bearer ") {
		t.Fatalf("timer admission leaked receiver configuration: %s", w.Body.String())
	}
	location := w.Header().Get("Location")
	if !strings.Contains(location, "scope_id=channel") {
		t.Fatal("queryable Location missing scope")
	}
	w = timerAPIRequest(router, "POST", "/timers", "key-a", in)
	if w.Code != 202 || len(st.rows) != 1 {
		t.Fatalf("duplicate admission: %d %s", w.Code, w.Body)
	}
	changed := in
	changed.OpaqueActionRef = "other"
	if w := timerAPIRequest(router, "POST", "/timers", "key-a", changed); w.Code != 409 {
		t.Fatalf("changed payload: %d", w.Code)
	} else if responseObject(t, w)["error_code"] != "idempotency_conflict" {
		t.Fatalf("timer conflict code: %s", w.Body.String())
	}
	if w := timerAPIRequest(router, "GET", location, "key-a", nil); w.Code != 200 {
		t.Fatalf("query: %d %s", w.Code, w.Body)
	}
	if w := timerAPIRequest(router, "GET", location, "key-b", nil); w.Code != 403 {
		t.Fatalf("cross tenant: %d", w.Code)
	} else if responseObject(t, w)["error_code"] != "tenant_mismatch" {
		t.Fatalf("cross tenant code: %s", w.Body.String())
	}
	wrongScope := strings.Replace(location, "scope_id=channel", "scope_id=private", 1)
	if w := timerAPIRequest(router, "GET", wrongScope, "key-a", nil); w.Code != 404 {
		t.Fatalf("cross scope: %d", w.Code)
	}
	wrongSubject := strings.Replace(location, "subject_ref=bot", "subject_ref=other", 1)
	if w := timerAPIRequest(router, "GET", wrongSubject, "key-a", nil); w.Code != 404 {
		t.Fatalf("cross subject: %d", w.Code)
	}
	changed = in
	changed.ScopeID = "private"
	if w := timerAPIRequest(router, "POST", "/timers", "key-a", changed); w.Code != 403 {
		t.Fatalf("unconfigured scope: %d", w.Code)
	} else if responseObject(t, w)["error_code"] != "timer_target_unavailable" {
		t.Fatalf("unconfigured target code: %s", w.Body.String())
	}
	var raw map[string]any
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &raw)
	raw["callback_url"] = "http://attacker/fire"
	if w := timerAPIRequest(router, "POST", "/timers", "key-a", raw); w.Code != 400 {
		t.Fatalf("model callback URL: %d", w.Code)
	} else if responseObject(t, w)["error_code"] != "invalid_request" {
		t.Fatalf("unknown callback field code: %s", w.Body.String())
	}
	encoded := in
	encoded.TimerID = "timer/é?100%"
	encoded.Revision = 2
	encodedResponse := timerAPIRequest(router, "POST", "/timers", "key-a", encoded)
	if encodedResponse.Code != 202 {
		t.Fatalf("encoded ID admission: %d", encodedResponse.Code)
	}
	if w := timerAPIRequest(router, "GET", encodedResponse.Header().Get("Location"), "key-a", nil); w.Code != 200 {
		t.Fatalf("encoded ID Location: %d %s", w.Code, w.Body)
	}
	st.failCreate = true
	changed = in
	changed.Revision++
	if w := timerAPIRequest(router, "POST", "/timers", "key-a", changed); w.Code != 500 {
		t.Fatalf("false durable ACK: %d", w.Code)
	} else if responseObject(t, w)["error_code"] != "internal_error" {
		t.Fatalf("store failure code: %s", w.Body.String())
	}
}

type timerSendFake struct {
	paths  []string
	bodies []timer.Handle
	fail   bool
}

func (s *timerSendFake) Send(_ context.Context, path string, body any) error {
	s.paths = append(s.paths, path)
	s.bodies = append(s.bodies, body.(timer.Handle))
	if s.fail {
		return errors.New("accepted response lost")
	}
	return nil
}

func TestTimerRecoveryRetainsStableSubmissionAfterLostResponse(t *testing.T) {
	st, router, in := timerAPIScenario(t)
	_, _, _ = st.CreateApplicationTimer(context.Background(), in, "bots", "digest")
	sender := &timerSendFake{fail: true}
	r := &TimerRecovery{Store: st, Ingress: sender}
	if err := r.Pass(context.Background()); err == nil {
		t.Fatal("lost response must be visible")
	}
	if st.submitted != 0 || st.rows[in.Key()].LastError == "" || st.rows[in.Key()].LastErrorCode != "timer_submit_failed" {
		t.Fatal("unknown submission was marked complete")
	}
	queryResponse := timerAPIRequest(router, "GET", "/timers/"+in.TimerID+"?tenant_id="+in.TenantID+"&subject_ref="+in.SubjectRef+"&scope_id="+in.ScopeID+"&revision=1", "key-a", nil)
	if queryResponse.Code != 200 || responseObject(t, queryResponse)["last_error_code"] != "timer_submit_failed" {
		t.Fatalf("timer query must expose machine error code: %d %s", queryResponse.Code, queryResponse.Body.String())
	}
	sender.fail = false
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.paths) != 2 || sender.paths[0] != sender.paths[1] || sender.bodies[0] != sender.bodies[1] || st.submitted != 1 {
		t.Fatalf("unstable resubmission: %+v", sender)
	}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.paths) != 2 {
		t.Fatal("submitted timer was polled as another due task")
	}
}
