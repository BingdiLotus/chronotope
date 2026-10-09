package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	mu               sync.Mutex
	orgs             map[string]bool
	orgRows          map[string]*store.Org
	agents           map[string]*store.Agent
	sessions         map[string]*store.Session
	runs             map[string]*store.Run
	msgs             []store.Message
	apiKeys          []*store.APIKeyRow
	deliverables     []*store.DeliverableRow
	subs             []map[string]string
	staleQueued      []*store.Run
	checkpoints      map[string]store.Checkpoint
	forks            []map[string]any
	rollbacks        []map[string]any
	watermark        int64
	usageBuckets     map[string]store.UsageRow
	archives         map[string]store.Archive
	users            map[string]store.User
	approvalPolicies map[string]store.ApprovalPolicy
	mcpAllowlists    map[string][]string
	knowledge        map[string]store.KnowledgeItem
	members          map[string]store.Member
	runStarted       map[string]time.Time
	pendingOutbox    []*store.PendingOutboxRow
	pendingType      string
	pendingPayload   []byte
	deliveredIDs     []int64
	retriedIDs       []int64
	events           []store.EventRow
	usage            []store.UsageRow
	summaries        []store.Summary
	memoryItems      []store.MemoryItem
	seq              int64
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
	if f.orgRows == nil {
		f.orgRows = map[string]*store.Org{}
	}
	f.orgRows[id] = &store.Org{ID: id, Name: name}
	return nil
}

func (f *fakeStore) CreateAgent(_ context.Context, id, orgID, name string, cfg *sessionapi.AgentConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg.Version = 1
	f.agents[id] = &store.Agent{ID: id, OrgID: orgID, Name: name, Config: *cfg, Version: 1, SpecDigest: store.SpecDigestOf(cfg)}
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

func (f *fakeStore) MarkAdmissionDispatched(_ context.Context, _ string) error { return nil }

func (f *fakeStore) ListPendingAdmissions(_ context.Context, _ time.Duration, _ int) ([]store.AdmissionPending, error) {
	return nil, nil
}

func (f *fakeStore) CreateRunWithCommand(_ context.Context, id, sessionID, input, topic string, trigger json.RawMessage, bound map[string]any) (bool, error) {
	return f.CreateRun(context.Background(), id, sessionID, trigger, bound)
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
	return f.CreateAPIKeyForUser(context.Background(), id, orgID, "", keyHash, scopes)
}

func (f *fakeStore) CreateAPIKeyForUser(_ context.Context, id, orgID, userID, keyHash string, scopes []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiKeys = append(f.apiKeys, &store.APIKeyRow{ID: id, OrgID: orgID, UserID: userID, KeyHash: keyHash, Scopes: scopes})
	return nil
}

func (f *fakeStore) LatestEventSeq(_ context.Context, _ string) (int64, error) { return 0, nil }
func (f *fakeStore) GetUsageWatermark(_ context.Context) (int64, error)        { return f.watermark, nil }
func (f *fakeStore) ApplyUsageDelta(_ context.Context, fromID, toID int64, deltas []store.UsageRow) error {
	for _, u := range deltas {
		key := u.SessionID + "|" + u.Bucket.Format(time.RFC3339)
		if f.usageBuckets == nil {
			f.usageBuckets = map[string]store.UsageRow{}
		}
		ex := f.usageBuckets[key]
		ex.SessionID, ex.Bucket = u.SessionID, u.Bucket
		ex.ActiveSeconds += u.ActiveSeconds
		ex.TokensIn += u.TokensIn
		ex.TokensOut += u.TokensOut
		ex.ComputeSeconds += u.ComputeSeconds
		f.usageBuckets[key] = ex
	}
	if f.watermark != fromID {
		return fmt.Errorf("watermark 竞态：期望 %d 实际 %d", fromID, f.watermark)
	}
	f.watermark = toID
	return nil
}
func (f *fakeStore) RunStartedAt(_ context.Context, runID string) (time.Time, error) {
	if f.runStarted != nil {
		if t, ok := f.runStarted[runID]; ok {
			return t, nil
		}
	}
	return time.Time{}, nil
}

func (f *fakeStore) RunStartedAtOld(_ context.Context, runID string) (time.Time, error) {
	for _, e := range f.events {
		if e.RunID == runID && e.Type == event.RunStarted {
			return e.At, nil
		}
	}
	return time.Time{}, store.ErrNotFound
}
func (f *fakeStore) AddMember(_ context.Context, m store.Member) error {
	if f.members == nil {
		f.members = map[string]store.Member{}
	}
	f.members[m.OrgID+"/"+m.UserID] = m
	return nil
}

func (f *fakeStore) ListMembers(_ context.Context, orgID string) ([]store.Member, error) {
	var out []store.Member
	for _, m := range f.members {
		if m.OrgID == orgID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeStore) RemoveMember(_ context.Context, orgID, userID string) error {
	delete(f.members, orgID+"/"+userID)
	return nil
}

func (f *fakeStore) AggregateOrgUsage(context.Context, string, string) ([]store.UsageAggregate, error) {
	return []store.UsageAggregate{{Bucket: time.Now().Truncate(time.Hour), TokensIn: 120, TokensOut: 80, ComputeSeconds: 2.5, ActiveSeconds: 30}}, nil
}

func (f *fakeStore) CreateKnowledge(_ context.Context, k store.KnowledgeItem) error {
	if f.knowledge == nil {
		f.knowledge = map[string]store.KnowledgeItem{}
	}
	f.knowledge[k.ID] = k
	return nil
}

func (f *fakeStore) UpsertMCPAllowlist(_ context.Context, tenantID, server string, patterns []string) error {
	if f.mcpAllowlists == nil {
		f.mcpAllowlists = map[string][]string{}
	}
	f.mcpAllowlists[tenantID+"|"+server] = patterns
	return nil
}

func (f *fakeStore) MCPToolAllowed(_ context.Context, _, _, _ string) (bool, error) { return true, nil }

func (f *fakeStore) UpsertApprovalPolicy(_ context.Context, p store.ApprovalPolicy) error {
	if f.approvalPolicies == nil {
		f.approvalPolicies = map[string]store.ApprovalPolicy{}
	}
	f.approvalPolicies[p.TenantID] = p
	return nil
}

func (f *fakeStore) ListOrgAuditEvents(_ context.Context, _ string, _ string, _ int) ([]store.AuditEvent, error) {
	return nil, nil
}

func (f *fakeStore) CreateUser(_ context.Context, u store.User) error {
	if f.users == nil {
		f.users = map[string]store.User{}
	}
	f.users[u.ID] = u
	return nil
}

func (f *fakeStore) CreateArchive(_ context.Context, a store.Archive) error {
	f.archives[a.SessionID] = a
	return nil
}
func (f *fakeStore) GetArchive(_ context.Context, sessionID string) (*store.Archive, error) {
	a, ok := f.archives[sessionID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &a, nil
}
func (f *fakeStore) SessionLastEventAt(_ context.Context, _ string) (time.Time, error) {
	return time.Now(), nil
}
func (f *fakeStore) CreateCheckpoint(_ context.Context, cp store.Checkpoint) (bool, error) {
	if f.checkpoints == nil {
		f.checkpoints = map[string]store.Checkpoint{}
	}
	f.checkpoints[cp.ID] = cp
	return true, nil
}
func (f *fakeStore) GetCheckpoint(_ context.Context, id string) (*store.Checkpoint, error) {
	cp, ok := f.checkpoints[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &cp, nil
}
func (f *fakeStore) ListCheckpoints(_ context.Context, _ string, _ int) ([]store.Checkpoint, error) {
	return nil, nil
}
func (f *fakeStore) ForkSession(_ context.Context, newID, parent string, atSeq int64, atCP string) error {
	f.forks = append(f.forks, map[string]any{"new": newID, "parent": parent, "at_seq": atSeq, "cp": atCP})
	return nil
}
func (f *fakeStore) RollbackSession(_ context.Context, sessionID string, cp *store.Checkpoint) error {
	f.rollbacks = append(f.rollbacks, map[string]any{"session": sessionID, "cp": cp.ID})
	return nil
}
func (f *fakeStore) DiffSessions(_ context.Context, _, _ string, _ int) (*store.SessionDiff, error) {
	return &store.SessionDiff{CommonPrefix: 1, OnlyA: []store.EventRow{{Seq: 2}}, OnlyB: []store.EventRow{{Seq: 2}}}, nil
}

func (f *fakeStore) ListAuditEventsByRun(_ context.Context, runID string, limit int) ([]store.AuditEventRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AuditEventRow
	for _, e := range f.events {
		if e.RunID == runID {
			out = append(out, store.AuditEventRow{Seq: e.Seq, Type: e.Type, At: e.At, DedupeKey: "dedupe:" + runID, Payload: e.Payload})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) ListStaleQueuedRuns(_ context.Context, _ time.Time, _ int) ([]*store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.staleQueued, nil
}

func (f *fakeStore) Subscribe(_ context.Context, sessionID, channel, target string) error {
	f.subs = append(f.subs, map[string]string{"session": sessionID, "channel": channel, "target": target})
	return nil
}

func (f *fakeStore) ListPendingOutbox(_ context.Context, _ int) ([]*store.PendingOutboxRow, error) {
	return f.pendingOutbox, nil
}

func (f *fakeStore) GetEvent(_ context.Context, eventID int64) (string, []byte, time.Time, error) {
	return f.pendingType, f.pendingPayload, time.Now(), nil
}

func (f *fakeStore) OutboxDelivered(_ context.Context, id int64) error {
	f.deliveredIDs = append(f.deliveredIDs, id)
	return nil
}

func (f *fakeStore) OutboxRetry(_ context.Context, id, _ int64) error {
	f.retriedIDs = append(f.retriedIDs, id)
	return nil
}

func (f *fakeStore) ListDeliverables(_ context.Context, _ string, _ int) ([]*store.DeliverableRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deliverables, nil
}

func (f *fakeStore) MarkDeliverableDelivered(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.deliverables {
		if row.ID == id {
			now := time.Now()
			row.DeliveredAt = &now
		}
	}
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
	mu      sync.Mutex
	calls   []string
	failErr error
	runOut  struct {
		Final    string `json:"final"`
		Steps    int    `json:"steps"`
		Canceled bool   `json:"canceled"`
	}
}

func (f *fakeIngress) Call(_ context.Context, path, method string, body any, out any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+path)
	f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	switch {
	case strings.HasPrefix(path, "/session_object/"):
		if m, ok := out.(*struct {
			Phase string `json:"phase"`
		}); ok {
			m.Phase = "ready"
		}
	case strings.HasPrefix(path, "/run_workflow/"):
		if m, ok := out.(*struct {
			Final    string `json:"final"`
			Steps    int    `json:"steps"`
			Canceled bool   `json:"canceled"`
		}); ok {
			m.Final, m.Steps, m.Canceled = f.runOut.Final, f.runOut.Steps, f.runOut.Canceled
		}
	}
	return nil
}

func (f *fakeIngress) allCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
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
		Final    string `json:"final"`
		Steps    int    `json:"steps"`
		Canceled bool   `json:"canceled"`
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
	rec := doJSON(t, h.Router(), http.MethodPost, "/webhooks/approval/r_1", `{"payload":"approve","approver":"alice","action_digest":"d1"}`, nil)
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

// TestListDeliveriesAndAck 交付清单查询 + 投递回执。
func TestListDeliveriesAndAck(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	fs.deliverables = []*store.DeliverableRow{{ID: 1, RunID: "r_1", SessionID: "s_seed", Kind: "run_completed", Payload: json.RawMessage(`{"final":"完成"}`)}}
	rec := doJSON(t, h.Router(), http.MethodGet, "/sessions/s_seed/deliveries", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "run_completed") {
		t.Fatalf("清单应含交付行: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/deliveries/1/ack", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ack 应 204，得 %d", rec.Code)
	}
	if fs.deliverables[0].DeliveredAt == nil {
		t.Fatal("ack 后应标记投递")
	}
}

// TestSubscribeValidation 订阅校验：webhook SSRF 拒绝 / email 格式。
func TestSubscribeValidation(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	// 内网地址拒绝（SSRF）
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/subscriptions",
		`{"channel":"webhook","target":"http://192.168.1.10/hook"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("内网 webhook 应 422，得 %d: %s", rec.Code, rec.Body.String())
	}
	// 非 http(s)
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/subscriptions",
		`{"channel":"webhook","target":"file:///etc/passwd"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非 http(s) 应 422，得 %d", rec.Code)
	}
	// 合法 webhook（allow-private 放行本地）
	h.DeliveryAllowPrivate = true
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/subscriptions",
		`{"channel":"webhook","target":"http://localhost:9999/hook"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("本地 webhook（放行）应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	// email 格式
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/subscriptions",
		`{"channel":"email","target":"ops@example.com"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("email 应 201，得 %d", rec.Code)
	}
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/subscriptions",
		`{"channel":"email","target":"not-an-email"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法 email 应 422，得 %d", rec.Code)
	}
}

// TestDelivererPass 投递循环：成功删行 / 失败退避（fake store + httptest 接收器）。
func TestDelivererPass(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "run.completed") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sink.Close()

	_, fs, _ := setup(t)
	fs.pendingType = "run.completed"
	fs.pendingPayload = []byte(`{"final":"完成"}`)
	fs.pendingOutbox = []*store.PendingOutboxRow{
		{ID: 1, SessionID: "s_seed", EventID: 1, URL: sink.URL, Channel: "webhook"},
		{ID: 2, SessionID: "s_seed", EventID: 2, URL: "ops@example.com", Channel: "email"}, // SMTP 未配置 → 失败退避
	}
	d := &Deliverer{Store: fs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Batch: 10}
	if err := d.pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(fs.deliveredIDs) != 1 || fs.deliveredIDs[0] != 1 {
		t.Fatalf("成功行应删除: %v", fs.deliveredIDs)
	}
	if len(fs.retriedIDs) != 1 || fs.retriedIDs[0] != 2 {
		t.Fatalf("失败行应退避: %v", fs.retriedIDs)
	}
}

// TestAdmissionRecovery 评审 #6：queued 遗留重投（崩溃窗口接纳屏障）。
func TestAdmissionRecovery(t *testing.T) {
	h, fs, ing := setup(t)
	fs.staleQueued = []*store.Run{
		{ID: "r_stale1", SessionID: "s_seed"},
		{ID: "r_stale2", SessionID: "s_seed"},
	}
	rec := &AdmissionRecovery{Store: fs, Ingress: h.Ingress, Stale: 5 * time.Minute}
	if err := rec.pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	calls := ing.allCalls()
	var redelivered int
	for _, c := range calls {
		if strings.Contains(c, "/run_workflow/r_stale") {
			redelivered++
		}
	}
	if redelivered != 2 {
		t.Fatalf("应重投 2 个遗留 run: %v", calls)
	}
}

// TestSubmitRunBindsConfigSnapshot ⑩：run 绑定 config 快照 + spec_digest + 协议版本。
func TestSubmitRunBindsConfigSnapshot(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/runs",
		`{"input":"x"}`, map[string]string{"Idempotency-Key": "k-snap"})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusAccepted {
		t.Fatalf("提交应成功: %d %s", rec.Code, rec.Body.String())
	}
	wantID := runIDFromIdempotency("s_seed", "k-snap")
	run, ok := fs.runs[wantID]
	if !ok {
		t.Fatalf("run 应入 fake（want %s）: %v", wantID, fs.runs)
	}
	b, _ := json.Marshal(run.Bound)
	var bound struct {
		SpecDigest  string                 `json:"spec_digest"`
		Protocol    string                 `json:"protocol_version"`
		AgentConfig sessionapi.AgentConfig `json:"agent_config"`
	}
	_ = json.Unmarshal(b, &bound)
	if bound.SpecDigest == "" || bound.Protocol != "1.0" || bound.AgentConfig.Model != "claude-sonnet-4-6" {
		t.Fatalf("bound 应含 digest/协议/快照: %s", b)
	}
}

// TestRunAuditEndpoint 期 1：journal 审计导出——重放轨迹 + dedupe 证据链 ndjson。
func TestRunAuditEndpoint(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	runID := runIDFromIdempotency("s_seed", "k-audit")
	fs.runs[runID] = &store.Run{ID: runID, SessionID: "s_seed", Status: sessionapi.RunCompleted}
	fs.events = []store.EventRow{
		{Seq: 1, RunID: runID, Type: event.RunStarted, At: time.Now()},
		{Seq: 2, RunID: runID, Type: event.SandboxExec, At: time.Now()},
	}
	rec := doJSON(t, h.Router(), http.MethodGet, "/runs/"+runID+"/audit", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit 应 200: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dedupe:"+runID) || !strings.Contains(rec.Body.String(), "sandbox.exec") {
		t.Fatalf("审计应含 dedupe 键与轨迹: %s", rec.Body.String())
	}
	// 未知 run → 404
	rec = doJSON(t, h.Router(), http.MethodGet, "/runs/r_none/audit", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 run 应 404: %d", rec.Code)
	}
}

// TestTimeTravelEndpoints 期 2：checkpoint/fork/rollback/diff 端点。
func TestTimeTravelEndpoints(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	fs.checkpoints = map[string]store.Checkpoint{
		"cp_1": {ID: "cp_1", SessionID: "s_seed", Seq: 5, SnapshotRef: "img|tar"},
	}
	// fork
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/fork", `{"checkpoint_id":"cp_1"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fork 应 201: %d %s", rec.Code, rec.Body.String())
	}
	if len(fs.forks) != 1 || fs.forks[0]["cp"] != "cp_1" {
		t.Fatalf("fork 应带血缘: %v", fs.forks)
	}
	// 未知 checkpoint → 404
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/fork", `{"checkpoint_id":"cp_x"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 checkpoint 应 404: %d", rec.Code)
	}
	// rollback
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/rollback", `{"checkpoint_id":"cp_1"}`, nil)
	if rec.Code != http.StatusOK || len(fs.rollbacks) != 1 {
		t.Fatalf("rollback 应 200: %d %s %v", rec.Code, rec.Body.String(), fs.rollbacks)
	}
	// diff
	rec = doJSON(t, h.Router(), http.MethodGet, "/sessions/s_seed/diff?against=s_other", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "common_prefix") {
		t.Fatalf("diff 应 200: %d %s", rec.Code, rec.Body.String())
	}
}

// TestArchiveEndpoints 期 2 §B：归档（活跃期 409/成功/清单/未启用 503）。
func TestArchiveEndpoints(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	h.ArchiveMinAge = time.Hour
	// 活跃期（fake 最后事件 = 现在）→ 409
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/archive", `{}`, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("活跃期应 409: %d %s", rec.Code, rec.Body.String())
	}
	// 超过最小年龄 → 归档成功（fake S3：Blob nil 则 503——此处只验证年龄闸门后的流程走通
	// 需 Blob：注入 fake？——单测验证 503 路径（无 S3 配置）与年龄闸门
	h.ArchiveMinAge = 0
	rec = doJSON(t, h.Router(), http.MethodPost, "/sessions/s_seed/archive", `{}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无 S3 配置应 503: %d %s", rec.Code, rec.Body.String())
	}
	// GET 清单：无归档 → 404
	rec = doJSON(t, h.Router(), http.MethodGet, "/sessions/s_seed/archive", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("无归档应 404: %d", rec.Code)
	}
	// 有清单 → 200
	if fs.archives == nil {
		fs.archives = map[string]store.Archive{}
	}
	fs.archives["s_seed"] = store.Archive{SessionID: "s_seed", BucketPath: "org/s_seed", EventsCount: 5, MessagesCount: 3}
	rec = doJSON(t, h.Router(), http.MethodGet, "/sessions/s_seed/archive", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bucket_path") {
		t.Fatalf("清单应 200: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPrincipalFlow 期 3 §A：principal 化——建主体 + key 绑 user + 认证注入 + user 限流。
func TestPrincipalFlow(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	// 建主体
	rec := doJSON(t, h.Router(), http.MethodPost, "/orgs/org_seed/users", `{"name":"张三"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建主体应 201: %d %s", rec.Code, rec.Body.String())
	}
	// key 绑 user
	rec = doJSON(t, h.Router(), http.MethodPost, "/orgs/org_seed/keys", `{"user_id":"u_x"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("key 应 201: %d %s", rec.Code, rec.Body.String())
	}
	// fake 的 key 行带 UserID（验证 CreateAPIKeyForUser 路径）
	found := false
	for _, v := range fs.apiKeys {
		if v.UserID == "u_x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("key 应绑 principal u_x: %+v", fs.apiKeys)
	}
}

// TestApprovalPolicyAndAuditEndpoints 期 3 §B：审批策略 upsert + 审计导出。
func TestApprovalPolicyAndAuditEndpoints(t *testing.T) {
	h, fs, _ := setup(t)
	seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPut, "/orgs/org_seed/approval-policy",
		`{"tool_patterns":[],"approvers":["alice","bob"],"ttl_seconds":3600}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("策略应 200: %d %s", rec.Code, rec.Body.String())
	}
	if fs.approvalPolicies["org_seed"].Approvers[0] != "alice" || fs.approvalPolicies["org_seed"].TTLSeconds != 3600 {
		t.Fatalf("策略应落 fake: %+v", fs.approvalPolicies["org_seed"])
	}
	rec = doJSON(t, h.Router(), http.MethodGet, "/orgs/org_seed/audit?kind=audit.", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "audit") {
		t.Fatalf("审计导出应 200: %d %s", rec.Code, rec.Body.String())
	}
}

// TestSubmitRunCanceledCanonical 审计 #9：workflow canceled 的返回写 canonical
// canceled（此前无条件 completed——HTTP 返回覆盖 workflow 权威状态的反例关闭）。
func TestSubmitRunCanceledCanonical(t *testing.T) {
	h, fs, ing := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	ing.runOut.Canceled = true
	ing.runOut.Final = "已取消"
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/runs",
		`{"input":"hi"}`, map[string]string{"Idempotency-Key": "k-cancel", "Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("提交应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != "canceled" {
		t.Fatalf("canceled workflow 应返回 canceled 状态: %+v err=%v", resp, err)
	}
}

// TestSubmitRunIngressErrorKeepsRunning 审计 #4：ingress 错误（同步超时/断开）
// 不再无条件写 failed——workflow 可能已被接纳继续运行（长审批反例：写
// failed 释放 active 唯一约束让另一 Run 进入）。
func TestSubmitRunIngressErrorKeepsRunning(t *testing.T) {
	h, fs, ing := setup(t)
	ing.failErr = fmt.Errorf("context deadline exceeded")
	_, sessionID := seedAgentSession(t, h, fs)
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/runs",
		`{"input":"hi"}`, map[string]string{"Idempotency-Key": "k-ingress", "Content-Type": "application/json"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ingress 错误应 502，得 %d", rec.Code)
	}
	// canonical 状态仍 running（未被 HTTP 错误覆盖为 failed）——fakeStore 的
	// run 由 CreateRun 建为 queued、submit 置 running；失败覆盖会写 failed
	failed := false
	fs.mu.Lock()
	for _, r := range fs.runs {
		if r.Status == sessionapi.RunFailed {
			failed = true
		}
	}
	fs.mu.Unlock()
	if failed {
		t.Fatal("ingress 错误不得把 run 写 failed（worker terminal 写是权威）")
	}
}
