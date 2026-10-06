package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"archive/tar"

	"github.com/go-chi/chi/v5"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/events"
	"github.com/bingdilotus/chronotope/internal/store"
)

// --- fakes ---

type fakeStore struct {
	mu          sync.Mutex
	orgs        map[string]bool
	orgRows     map[string]*store.Org
	agents      map[string]*store.Agent
	sessions    map[string]*store.Session
	runs        map[string]*store.Run
	msgs        []store.Message
	apiKeys     []*store.APIKeyRow
	events      []store.EventRow
	usage       []store.UsageRow
	summaries   []store.Summary
	memoryItems []store.MemoryItem
	seq         int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		orgs:     map[string]bool{},
		agents:   map[string]*store.Agent{},
		sessions: map[string]*store.Session{},
		runs:     map[string]*store.Run{},
	}
}

func (f *fakeStore) CreateOrg(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs[id] = true
	return nil
}

func (f *fakeStore) CreateAgent(_ context.Context, id, orgID, name string, cfg *sessionapi.AgentConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg.Version = 1
	f.agents[id] = &store.Agent{ID: id, OrgID: orgID, Name: name, Config: *cfg, Version: 1}
	return nil
}

func (f *fakeStore) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return a, nil
}

func (f *fakeStore) CreateSession(_ context.Context, id, orgID, agentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id] = &store.Session{ID: id, OrgID: orgID, AgentID: agentID, Status: sessionapi.PhaseCreated}
	return nil
}

func (f *fakeStore) GetSession(_ context.Context, id string) (*store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeStore) UpdateSessionStatus(_ context.Context, id string, status sessionapi.SessionPhase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		s.Status = status
		now := time.Now()
		s.LastActiveAt = &now
	}
	return nil
}

func (f *fakeStore) SoftDeleteSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		s.Status = sessionapi.PhaseDeleted
		now := time.Now()
		s.DeletedAt = &now
	}
	return nil
}

func (f *fakeStore) CreateRun(_ context.Context, id, sessionID string, trigger json.RawMessage, bound map[string]any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.runs[id]; ok {
		return false, nil
	}
	f.runs[id] = &store.Run{ID: id, SessionID: sessionID, Status: sessionapi.RunQueued, Bound: bound}
	return true, nil
}

func (f *fakeStore) GetRun(_ context.Context, id string) (*store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) UpdateRunStatus(_ context.Context, id string, status sessionapi.RunStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[id]; ok {
		r.Status = status
	}
	return nil
}

func (f *fakeStore) CreateSchedule(_ context.Context, id, orgID, sessionID string, delay time.Duration, payload json.RawMessage) error {
	return nil
}

func (f *fakeStore) GetOrg(_ context.Context, orgID string) (*store.Org, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.orgRows == nil {
		f.orgRows = map[string]*store.Org{}
	}
	if o, ok := f.orgRows[orgID]; ok {
		return o, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) UpdateOrgQuotas(_ context.Context, orgID string, quotas map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.orgRows == nil {
		f.orgRows = map[string]*store.Org{}
	}
	if o, ok := f.orgRows[orgID]; !ok {
		return store.ErrNotFound
	} else {
		if o.Quotas == nil {
			o.Quotas = map[string]any{}
		}
		for k, v := range quotas {
			o.Quotas[k] = v
		}
	}
	return nil
}

func (f *fakeStore) GetActiveRun(_ context.Context, sessionID string) (*store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.SessionID == sessionID &&
			(r.Status == sessionapi.RunQueued || r.Status == sessionapi.RunRunning ||
				r.Status == sessionapi.RunPaused || r.Status == sessionapi.RunAwaitingApproval ||
				r.Status == sessionapi.RunFrozen) {
			return r, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListEventsAfterID(_ context.Context, afterID int64, limit int) ([]store.EventRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.EventRow
	for _, e := range f.events {
		if e.ID > afterID && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) ListSessionsByOrg(_ context.Context, orgID string, limit int) ([]store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Session
	for _, sess := range f.sessions {
		if sess.OrgID == orgID && len(out) < limit {
			out = append(out, *sess)
		}
	}
	return out, nil
}

func (f *fakeStore) ResetSessionUsage(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = nil
	return nil
}

func (f *fakeStore) UpsertUsage(_ context.Context, u store.UsageRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, existing := range f.usage {
		if existing.SessionID == u.SessionID && existing.Bucket.Equal(u.Bucket) {
			f.usage[i].ActiveSeconds += u.ActiveSeconds
			f.usage[i].TokensIn += u.TokensIn
			f.usage[i].TokensOut += u.TokensOut
			f.usage[i].ComputeSeconds += u.ComputeSeconds
			return nil
		}
	}
	f.usage = append(f.usage, u)
	return nil
}

func (f *fakeStore) GetAPIKeyByHash(_ context.Context, keyHash string) (*store.APIKeyRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.apiKeys {
		if k.KeyHash == keyHash {
			return k, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) CreateAPIKey(_ context.Context, id, orgID, keyHash string, scopes []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiKeys = append(f.apiKeys, &store.APIKeyRow{ID: id, OrgID: orgID, KeyHash: keyHash, Scopes: scopes})
	return nil
}

func (f *fakeStore) ListMessages(_ context.Context, _ string, limit int) ([]store.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Message
	for _, m := range f.msgs {
		if len(out) < limit {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeStore) ListSummaries(_ context.Context, sessionID string) ([]store.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summaries, nil
}

func (f *fakeStore) ListMemoryItems(_ context.Context, sessionID, topic string, limit int) ([]store.MemoryItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.MemoryItem
	for _, it := range f.memoryItems {
		if it.SessionID == sessionID && (topic == "" || it.Topic == topic) && len(out) < limit {
			out = append(out, it)
		}
	}
	return out, nil
}

func (f *fakeStore) ListUsage(_ context.Context, sessionID string) ([]store.UsageRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.UsageRow
	for _, u := range f.usage {
		if u.SessionID == sessionID {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeStore) ListEvents(_ context.Context, sessionID string, afterSeq int64, limit int) ([]store.EventRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.EventRow
	for _, e := range f.events {
		if e.SessionID == sessionID && e.Seq > afterSeq && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) addEvent(sessionID string, typ event.Type) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	e := store.EventRow{ID: f.seq, SessionID: sessionID, Seq: f.seq, Type: typ, Payload: json.RawMessage(`{"v":1}`), At: time.Now()}
	f.events = append(f.events, e)
	return f.seq
}

// fakeIngress 记录调用并按路径回放结果。
type fakeIngress struct {
	mu     sync.Mutex
	calls  []string
	runOut struct {
		Final string `json:"final"`
		Steps int    `json:"steps"`
	}
}

func (f *fakeIngress) Call(_ context.Context, path, method string, body any, out any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+path)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(path, "/session_object/"):
		if m, ok := out.(*struct {
			Phase string `json:"phase"`
		}); ok {
			m.Phase = "ready"
		}
	case strings.HasPrefix(path, "/run_workflow/"):
		if m, ok := out.(*struct {
			Final string `json:"final"`
			Steps int    `json:"steps"`
		}); ok {
			m.Final, m.Steps = f.runOut.Final, f.runOut.Steps
		}
	}
	return nil
}

func (f *fakeIngress) lastCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeIngress) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- 测试辅助 ---

func setup(t *testing.T) (*Handler, *fakeStore, *fakeIngress) {
	t.Helper()
	fs := newFakeStore()
	ing := &fakeIngress{runOut: struct {
		Final string `json:"final"`
		Steps int    `json:"steps"`
	}{Final: "你好，我是助手。", Steps: 1}}
	h := New(fs, events.NewHub(), ing)
	return h, fs, ing
}

func seedAgentSession(t *testing.T, h *Handler, fs *fakeStore) (agentID, sessionID string) {
	t.Helper()
	agentID = "a_seed"
	sessionID = "s_seed"
	_ = fs.CreateOrg(context.Background(), "org_seed", "org")
	_ = fs.CreateAgent(context.Background(), agentID, "org_seed", "agent",
		&sessionapi.AgentConfig{Model: "claude-sonnet-4-6", Instructions: "你是助手。", Version: 1})
	_ = fs.CreateSession(context.Background(), sessionID, "org_seed", agentID)
	_ = fs.UpdateSessionStatus(context.Background(), sessionID, sessionapi.PhaseReady)
	return agentID, sessionID
}

func doJSON(t *testing.T, h http.Handler, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- 用例 ---

func TestCreateAgent(t *testing.T) {
	h, _, _ := setup(t)
	rec := doJSON(t, h.Router(), http.MethodPost, "/orgs/org1/agents",
		`{"name":"coder","config":{"model":"claude-sonnet-4-6","instructions":"你是编码助手。","tools":["bash"],"version":1}}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建 agent 应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID     string                 `json:"id"`
		Config sessionapi.AgentConfig `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	if resp.ID == "" || resp.Config.Version != 1 || resp.Config.Model != "claude-sonnet-4-6" {
		t.Fatalf("agent 响应不符: %+v", resp)
	}
}

func TestCreateSessionWiresSessionObject(t *testing.T) {
	h, fs, ing := setup(t)
	agentID, _ := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/agents/"+agentID+"/sessions", "", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建 session 应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	if last := ing.lastCall(); !strings.HasPrefix(last, "POST /session_object/") || !strings.HasSuffix(last, "/Create") {
		t.Fatalf("应经控制面初始化 session_object: %q", last)
	}
	var resp struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status != "ready" {
		t.Fatalf("session 应 ready: %+v", resp)
	}
}

func TestSubmitRunIdempotency(t *testing.T) {
	h, fs, ing := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	path := "/sessions/" + sessionID + "/runs"
	body := `{"input":"帮我写个 hello world"}`
	headers := map[string]string{"Idempotency-Key": "k-1", "Content-Type": "application/json"}

	rec1 := doJSON(t, h.Router(), http.MethodPost, path, body, headers)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("首次提交应 201，得 %d: %s", rec1.Code, rec1.Body.String())
	}
	var r1 struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
		Final  string `json:"final"`
	}
	_ = json.Unmarshal(rec1.Body.Bytes(), &r1)
	if r1.RunID == "" || r1.Status != "completed" || r1.Final == "" {
		t.Fatalf("run 响应不符: %+v", r1)
	}
	if got := ing.callCount(); got != 1 {
		t.Fatalf("ingress 应只触发 1 次 run_workflow，得 %d", got)
	}

	// 同幂等键重放：返回同一 run_id，不重复触发
	rec2 := doJSON(t, h.Router(), http.MethodPost, path, body, headers)
	if rec2.Code != http.StatusOK {
		t.Fatalf("幂等重放应 200，得 %d: %s", rec2.Code, rec2.Body.String())
	}
	var r2 struct {
		RunID      string `json:"run_id"`
		Idempotent bool   `json:"idempotent"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &r2)
	if r2.RunID != r1.RunID || !r2.Idempotent {
		t.Fatalf("幂等键应返回同一 run_id 且标记幂等: %+v", r2)
	}
	if got := ing.callCount(); got != 1 {
		t.Fatalf("幂等重放不应重复触发 run_workflow，得 %d", got)
	}

	// 不同幂等键 → 新 run_id
	rec3 := doJSON(t, h.Router(), http.MethodPost, path, body, map[string]string{"Idempotency-Key": "k-2", "Content-Type": "application/json"})
	var r3 struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(rec3.Body.Bytes(), &r3)
	if r3.RunID == r1.RunID {
		t.Fatal("不同幂等键应产生不同 run_id")
	}
}

func TestSubmitRunRequiresIdempotencyKey(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/runs", `{"input":"x"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 Idempotency-Key 应 400，得 %d", rec.Code)
	}
}

func TestSubmitRunSessionNotFound(t *testing.T) {
	h, _, _ := setup(t)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/s_missing/runs", `{"input":"x"}`,
		map[string]string{"Idempotency-Key": "k"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 session 应 404，得 %d", rec.Code)
	}
}

func TestGetSessionWithRecentEvents(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	fs.addEvent(sessionID, event.RunStarted)
	rec := doJSON(t, h.Router(), http.MethodGet, "/sessions/"+sessionID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d", rec.Code)
	}
	var resp struct {
		Status       string `json:"status"`
		RecentEvents []struct {
			Type string `json:"type"`
		} `json:"recent_events"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status != "ready" || len(resp.RecentEvents) != 1 || resp.RecentEvents[0].Type != "run.started" {
		t.Fatalf("get session 响应不符: %+v", resp)
	}
}

func TestDeleteSessionTombstone(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodDelete, "/sessions/"+sessionID, "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("删除应 204，得 %d", rec.Code)
	}
	sess, _ := fs.GetSession(context.Background(), sessionID)
	if sess.Status != sessionapi.PhaseDeleted || sess.DeletedAt == nil {
		t.Fatalf("应 tombstone: %+v", sess)
	}
	// 已删除的 session 提交任务 → 404
	rec2 := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/runs", `{"input":"x"}`,
		map[string]string{"Idempotency-Key": "k"})
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("已删除 session 提交应 404，得 %d", rec2.Code)
	}
}

func TestStreamEventsBacklogAndLive(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	fs.addEvent(sessionID, event.RunStarted)
	fs.addEvent(sessionID, event.RunCompleted) // 历史两条（seq 1、2）

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartPoller(ctx) // poller 驱动 hub 提示

	// 请求上下文与 cancel 联动：取消后 SSE handler 必须退出（连接生命周期）
	req := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID+"/events?after=1", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Router().ServeHTTP(rec, req)
	}()

	// 等 backlog（after=1 → 只应补读 seq=2）与心跳帧写出
	waitFor(t, done, func() bool { return strings.Contains(rec.Body.String(), `"seq":2`) })
	time.Sleep(200 * time.Millisecond)

	// 新事件经 poller → hub → 回查推送（容忍 gap：直接跳到 seq=5）
	seq5 := fs.addEvent(sessionID, event.RunFailed)
	waitFor(t, done, func() bool {
		return strings.Contains(rec.Body.String(), fmt.Sprintf(`"seq":%d`, seq5))
	})

	// after=1 的历史补读不应包含 seq=1
	if strings.Contains(rec.Body.String(), `"seq":1`) {
		t.Fatalf("after=1 不应补读 seq=1: %s", rec.Body.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler 未随 ctx 取消退出")
	}
}

func TestStreamEventsSessionNotFound(t *testing.T) {
	h, _, _ := setup(t)
	rec := doJSON(t, h.Router(), http.MethodGet, "/sessions/s_missing/events", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 session 应 404，得 %d", rec.Code)
	}
}

func TestStreamEventsBadAfter(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodGet, "/sessions/"+sessionID+"/events?after=abc", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 after 应 400，得 %d", rec.Code)
	}
}

func waitFor(t *testing.T, done <-chan struct{}, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

func TestSessionActionPause(t *testing.T) {
	h, fs, ing := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/actions", `{"action":"pause"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pause 应 202，得 %d: %s", rec.Code, rec.Body.String())
	}
	if last := ing.lastCall(); !strings.HasPrefix(last, "POST /session_object/"+sessionID+"/Pause") {
		t.Fatalf("应转发 session_object.Pause: %q", last)
	}
}

func TestSessionActionUnknown(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/actions", `{"action":"teleport"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("未知动作应 422，得 %d", rec.Code)
	}
}

func TestApprovalWebhook(t *testing.T) {
	h, _, ing := setup(t)
	rec := doJSON(t, h.Router(), http.MethodPost, "/webhooks/approval/r_1", `{"payload":"approve"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("审批回调应 200，得 %d: %s", rec.Code, rec.Body.String())
	}
	if last := ing.lastCall(); last != "POST /webhook/Resolve" {
		t.Fatalf("应转发 webhook.Resolve: %q", last)
	}
}

func TestCreateSchedule(t *testing.T) {
	h, fs, ing := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/schedules",
		`{"delay_ms":1000,"payload":{"input":"定时任务"}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("建 schedule 应 202，得 %d: %s", rec.Code, rec.Body.String())
	}
	if last := ing.lastCall(); !strings.HasPrefix(last, "POST /scheduler/") || !strings.HasSuffix(last, "/run") {
		t.Fatalf("应触发 scheduler workflow: %q", last)
	}
}

func TestCreateScheduleInvalid(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/schedules", `{"delay_ms":0}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法 schedule 应 422，得 %d", rec.Code)
	}
}

func TestGetMemory(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	fs.summaries = []store.Summary{{SessionID: sessionID, Topic: "default", Version: 1, Summary: "摘要一", CreatedByRun: "r_1"}}
	fs.memoryItems = []store.MemoryItem{{SessionID: sessionID, Topic: "default", Kind: "long_term", Content: "用户偏好：Go", SourceRunID: "r_1", SourceStep: 3}}

	rec := doJSON(t, h.Router(), http.MethodGet, "/sessions/"+sessionID+"/memory", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "摘要一") || !strings.Contains(rec.Body.String(), "用户偏好：Go") {
		t.Fatalf("memory 应含摘要与条目: %s", rec.Body.String())
	}
}

func TestCreateAgentToolClassesPassthrough(t *testing.T) {
	h, _, _ := setup(t)
	rec := doJSON(t, h.Router(), http.MethodPost, "/orgs/org-rc/agents", `{
		"name":"rc-agent",
		"config":{"model":"m","instructions":"i","tools":["read_file","bash"],
			"tool_classes":{"bash":2},"version":1}}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Config sessionapi.AgentConfig `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if out.Config.ToolClasses["bash"] != 2 {
		t.Fatalf("tool_classes 应透传: %+v", out.Config.ToolClasses)
	}
}

func TestUpdateOrgBudget(t *testing.T) {
	h, fs, _ := setup(t)
	fs.orgRows = map[string]*store.Org{"org-b1": {ID: "org-b1", Name: "o", Quotas: map[string]any{}}}
	rec := doJSON(t, h.Router(), http.MethodPut, "/orgs/org-b1/budget",
		`{"daily_token_budget":5000,"daily_compute_seconds":600}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d: %s", rec.Code, rec.Body.String())
	}
	if fs.orgRows["org-b1"].Quotas[store.QuotaDailyTokenBudget] != float64(5000) {
		t.Fatalf("quotas 应更新: %+v", fs.orgRows["org-b1"].Quotas)
	}
	// org 不存在 → 404
	rec = doJSON(t, h.Router(), http.MethodPut, "/orgs/org-missing/budget", `{"daily_token_budget":1}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("org 不存在应 404，得 %d", rec.Code)
	}
}

func TestSessionActionUnfreeze(t *testing.T) {
	h, fs, ing := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/actions", `{"action":"unfreeze"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unfreeze 应 202，得 %d: %s", rec.Code, rec.Body.String())
	}
	if last := ing.lastCall(); !strings.HasSuffix(last, "/session_object/"+sessionID+"/Unfreeze") {
		t.Fatalf("应调 Unfreeze 对象方法: %q", last)
	}
}

func TestCorsMiddleware(t *testing.T) {
	router := chi.NewRouter()
	router.Use(CorsMiddleware(""))
	router.Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("默认源应放行: %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	// 非白名单源不放行
	req2 := httptest.NewRequest(http.MethodGet, "/x", nil)
	req2.Header.Set("Origin", "http://evil.example")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("非白名单源不应放行: %q", rec2.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCreateSessionWithParticipants(t *testing.T) {
	h, fs, ing := setup(t)
	agentID, _ := seedAgentSession(t, h, fs)
	// 群聊创建：participants 透传 + SetParticipants 对象调用
	rec := doJSON(t, h.Router(), http.MethodPost, "/agents/"+agentID+"/sessions", `{"participants":[{"agent_id":"a_m1","role":"架构师"}]}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a_m1") {
		t.Fatalf("participants 应回显: %s", rec.Body.String())
	}
	last := ing.lastCall()
	if !strings.HasSuffix(last, "/SetParticipants") {
		t.Fatalf("应调 SetParticipants: %q", last)
	}
}

func TestExportSessionTar(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	fs.addEvent("s_seed", event.RunCompleted)
	fs.msgs = []store.Message{{SessionID: sessionID, RunID: "r_1", Step: 0, Role: "assistant", Content: json.RawMessage(`"你好"`)}}
	fs.summaries = []store.Summary{{SessionID: sessionID, Topic: "default", Version: 1, Summary: "摘要"}}

	rec := doJSON(t, h.Router(), http.MethodGet, "/sessions/"+sessionID+"/export", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d: %s", rec.Code, rec.Body.String())
	}
	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	entries := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		b, _ := io.ReadAll(tr)
		entries[hdr.Name] = b
	}
	for _, name := range []string{"manifest.json", "events.ndjson", "messages.ndjson", "memory.json", "usage.csv"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("缺条目 %s: %v", name, keysOf(entries))
		}
	}
	var manifest struct {
		Counts struct {
			Events int `json:"events"`
		} `json:"counts"`
	}
	_ = json.Unmarshal(entries["manifest.json"], &manifest)
	if manifest.Counts.Events != 1 || !strings.Contains(string(entries["events.ndjson"]), "run.completed") {
		t.Fatalf("manifest/events 不符: %s %s", entries["manifest.json"], entries["events.ndjson"])
	}
}

func keysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAuthMiddlewareMode 认证矩阵：off 匿名放行 / on 强制 + org 归属 403。
func TestAuthMiddlewareMode(t *testing.T) {
	h, fs, _ := setup(t)
	keyPlain := "ck_test_abc"
	fs.apiKeys = append(fs.apiKeys, &store.APIKeyRow{ID: "k_1", OrgID: "org_seed", KeyHash: sha256Hex(keyPlain)})

	on := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	routerOn := chi.NewRouter()
	routerOn.Use(h.AuthMiddleware("on", ""))
	routerOn.Get("/orgs/{orgID}/x", on)
	routerOn.Get("/healthz", on)

	// 无 key → 401
	rec := httptest.NewRecorder()
	routerOn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orgs/org_seed/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("on 模式无 key 应 401，得 %d", rec.Code)
	}
	// 错 key → 401
	req := httptest.NewRequest(http.MethodGet, "/orgs/org_seed/x", nil)
	req.Header.Set("Authorization", "Bearer ck_wrong")
	rec = httptest.NewRecorder()
	routerOn.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 key 应 401，得 %d", rec.Code)
	}
	// 正确 key 但 org 不匹配 → 403
	req = httptest.NewRequest(http.MethodGet, "/orgs/other/x", nil)
	req.Header.Set("Authorization", "Bearer "+keyPlain)
	rec = httptest.NewRecorder()
	routerOn.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("org 归属不符应 403，得 %d", rec.Code)
	}
	// 正确 key + org 匹配 → 200
	req = httptest.NewRequest(http.MethodGet, "/orgs/org_seed/x", nil)
	req.Header.Set("Authorization", "Bearer "+keyPlain)
	rec = httptest.NewRecorder()
	routerOn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("正确 key 应 200，得 %d", rec.Code)
	}
	// 匿名路径（healthz）不受 on 限制
	rec = httptest.NewRecorder()
	routerOn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz 应放行，得 %d", rec.Code)
	}
	// off 模式：无 key 匿名放行
	routerOff := chi.NewRouter()
	routerOff.Use(h.AuthMiddleware("off", ""))
	routerOff.Get("/orgs/{orgID}/x", on)
	rec = httptest.NewRecorder()
	routerOff.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orgs/org_seed/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("off 模式应匿名放行，得 %d", rec.Code)
	}
}

// TestCreateAPIKey 生成 key：明文一次 + 库存哈希。
func TestCreateAPIKey(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	fs.orgRows = map[string]*store.Org{"org_seed": {ID: "org_seed", Name: "org"}}
	rec := doJSON(t, h.Router(), http.MethodPost, "/orgs/org_seed/keys", "", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Key, "ck_") || len(out.Key) < 32 {
		t.Fatalf("key 格式不符: %q", out.Key)
	}
	var stored *store.APIKeyRow
	for _, k := range fs.apiKeys {
		if k.KeyHash == sha256Hex(out.Key) {
			stored = k
		}
	}
	if stored == nil || stored.KeyHash == out.Key {
		t.Fatalf("库存应为哈希（非明文）: %+v", stored)
	}
}

// TestAuthMiddlewareAdminKey 管理面引导 key 全放行（on 模式）。
func TestAuthMiddlewareAdminKey(t *testing.T) {
	h, _, _ := setup(t)
	on := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	router := chi.NewRouter()
	router.Use(h.AuthMiddleware("on", "ck_admin"))
	router.Get("/orgs/{orgID}/x", on)
	req := httptest.NewRequest(http.MethodGet, "/orgs/any-org/x", nil)
	req.Header.Set("Authorization", "Bearer ck_admin")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin key 应全放行，得 %d", rec.Code)
	}
}

// fakeExecutorClient 记录 skill 安装的 executor 调用。
type fakeExecutorClient struct {
	paths   []string
	sandbox string
}

func (f *fakeExecutorClient) WriteFile(_ context.Context, path, content string) error {
	f.paths = append(f.paths, "PUT "+path)
	return nil
}

func (f *fakeExecutorClient) Call(_ context.Context, path, method string, body, out any) error {
	f.paths = append(f.paths, method+" "+path)
	if out != nil {
		if sb, ok := out.(*struct {
			SandboxID string `json:"sandbox_id"`
		}); ok && f.sandbox == "" {
			f.sandbox = "sb_skill_test"
			sb.SandboxID = f.sandbox
		}
	}
	return nil
}

// TestInstallSkill 沙箱文件写入 + 对象 AddSkill + 事件发布。
func TestInstallSkill(t *testing.T) {
	h, fs, ing := setup(t)
	seedAgentSession(t, h, fs)
	h.Executor = &fakeExecutorClient{}
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/skills",
		`{"name":"data-tools","content":"# 数据工具\n用法说明。"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	if last := ing.lastCall(); !strings.HasSuffix(last, "/InstallSkill") {
		t.Fatalf("应调 session_ops InstallSkill: %q", last)
	}
	ex := h.Executor.(*fakeExecutorClient)
	if len(ex.paths) != 2 || !strings.Contains(ex.paths[1], "PUT /files/sb_skill_test/workspace/skills/data-tools/SKILL.md") {
		t.Fatalf("executor 应建沙箱并写 SKILL.md: %v", ex.paths)
	}
	// 非法 name（路径分隔符）
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/skills",
		`{"name":"../etc","content":"x"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("路径穿越 name 应 422，得 %d", rec.Code)
	}
}

// TestConnectMCP 连接注册：URL 校验 + 对象 ConnectMCP。
func TestConnectMCP(t *testing.T) {
	h, fs, ing := setup(t)
	seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/mcp",
		`{"server":"echo","url":"http://mcp.example"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	if last := ing.lastCall(); !strings.HasSuffix(last, "/ConnectMCP") {
		t.Fatalf("应调 session_ops ConnectMCP: %q", last)
	}
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/mcp",
		`{"server":"bad","url":"file:///etc"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非 http(s) URL 应 422，得 %d", rec.Code)
	}
}
