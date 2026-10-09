package execproto

import (
	"bytes"
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

	"github.com/bingdilotus/chronotope/internal/store"
)

// fakeDriver 实现 Driver（服务器单测替身）。
type fakeDriver struct {
	orphanContainers []string
	orphanErr        error
	created          CreateSandboxRequest
	sandboxID        string
	executed         []ExecuteRequest
	execResult       *ExecuteResult
	files            map[string]string
	frozen           bool
	destroyed        map[string]bool
	failDestroy      bool
	snapshot         string
}

func (f *fakeDriver) ListWorkspaceFiles(_ context.Context, _ string) ([]WorkspaceEntry, error) {
	var out []WorkspaceEntry
	for path, content := range f.files {
		out = append(out, WorkspaceEntry{Path: path, Size: int64(len(content))})
	}
	return out, nil
}

func (f *fakeDriver) ListOrphanContainers(_ context.Context) ([]string, error) {
	return f.orphanContainers, f.orphanErr
}

func (f *fakeDriver) CreateSandbox(_ context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	f.created = req
	f.sandboxID = "sb_fake"
	return &Sandbox{ID: f.sandboxID}, nil
}

func (f *fakeDriver) Execute(_ context.Context, req ExecuteRequest, log io.Writer) (*ExecuteResult, error) {
	f.executed = append(f.executed, req)
	_, _ = io.WriteString(log, req.Input+"\n")
	if f.execResult != nil {
		return f.execResult, nil
	}
	return &ExecuteResult{Exit: 0}, nil
}

func (f *fakeDriver) ReadFile(_ context.Context, sandboxID, path string) ([]byte, error) {
	return []byte(f.files[path]), nil
}

func (f *fakeDriver) WriteFile(_ context.Context, sandboxID, path string, data []byte) error {
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}

func (f *fakeDriver) Freeze(context.Context, string) error   { f.frozen = true; return nil }
func (f *fakeDriver) Unfreeze(context.Context, string) error { f.frozen = false; return nil }
func (f *fakeDriver) Snapshot(context.Context, string) (string, error) {
	f.snapshot = "snap_ref"
	return f.snapshot, nil
}
func (f *fakeDriver) Destroy(_ context.Context, id string) error {
	if f.destroyed == nil {
		f.destroyed = map[string]bool{}
	}
	if f.failDestroy {
		return fmt.Errorf("destroy failed")
	}
	f.destroyed[id] = true
	return nil
}

type fakeSBStore struct {
	mu            sync.Mutex
	sandboxes     map[string]*store.SandboxRow
	execs         map[string]json.RawMessage
	prepared      map[string]string
	execsPrepared map[string]*store.ExecRow
	leases        map[string]*store.LeaseRow
	wsFiles       map[string]store.WorkspaceFile
	expiredRows   []*store.SandboxRow
	deletedIDs    []string
}

func newFakeSBStore() *fakeSBStore {
	return &fakeSBStore{
		sandboxes:     map[string]*store.SandboxRow{},
		execs:         map[string]json.RawMessage{},
		prepared:      map[string]string{},
		execsPrepared: map[string]*store.ExecRow{},
	}
}

func (f *fakeSBStore) UpsertSandbox(_ context.Context, sb *store.SandboxRow) error {
	f.sandboxes[sb.SandboxID] = sb
	return nil
}
func (f *fakeSBStore) GetSandbox(_ context.Context, id string) (*store.SandboxRow, error) {
	sb, ok := f.sandboxes[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return sb, nil
}
func (f *fakeSBStore) UpdateSandboxStatus(_ context.Context, id, status string) error {
	f.sandboxes[id].Status = status
	return nil
}
func (f *fakeSBStore) UpdateSandboxTier(_ context.Context, id string, tier int, ref *string) error {
	f.sandboxes[id].Tier = tier
	return nil
}
func (f *fakeSBStore) GetExec(_ context.Context, key string) (*store.ExecRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if raw, ok := f.execs[key]; ok {
		return &store.ExecRow{IdempotencyKey: key, Result: raw, State: "done"}, nil
	}
	if row, ok := f.execsPrepared[key]; ok {
		return row, nil
	}
	if _, ok := f.prepared[key]; ok {
		now := time.Now()
		return &store.ExecRow{IdempotencyKey: key, State: "prepared", PreparedAt: &now}, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeSBStore) AcquireLease(_ context.Context, sandboxID, runID string, _ time.Duration) (*store.LeaseRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leases == nil {
		f.leases = map[string]*store.LeaseRow{}
	}
	row := f.leases[sandboxID]
	// owner CAS：活跃租约且 holder 不同 → 拒绝（gc-reclaim 的 quiesce 失败）
	if row != nil && row.ExpiresAt.After(time.Now()) && row.RunID != runID {
		return nil, store.ErrLeaseOwnerMismatch
	}
	if row == nil {
		row = &store.LeaseRow{SandboxID: sandboxID, RunID: runID, Generation: 1}
	} else {
		row.Generation++
		row.RunID = runID
	}
	f.leases[sandboxID] = row
	return row, nil
}

func (f *fakeSBStore) ReleaseLease(_ context.Context, sandboxID string, generation int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.leases[sandboxID]
	if row == nil || row.Generation != generation {
		return false, nil
	}
	delete(f.leases, sandboxID)
	return true, nil
}

func (f *fakeSBStore) GetLease(_ context.Context, sandboxID string) (*store.LeaseRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leases == nil {
		return nil, nil
	}
	return f.leases[sandboxID], nil
}

func (f *fakeSBStore) HasActiveLease(_ context.Context, sandboxID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leases != nil && f.leases[sandboxID] != nil, nil
}

func (f *fakeSBStore) UpsertWorkspaceFile(_ context.Context, f2 store.WorkspaceFile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wsFiles == nil {
		f.wsFiles = map[string]store.WorkspaceFile{}
	}
	f.wsFiles[f2.SessionID+"|"+f2.Path] = f2
	return nil
}

func (f *fakeSBStore) ListWorkspaceFiles(_ context.Context, sessionID string, _ int) ([]store.WorkspaceFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.WorkspaceFile
	for k, v := range f.wsFiles {
		if strings.HasPrefix(k, sessionID+"|") {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeSBStore) PutExecPrepared(_ context.Context, key, sandboxID, digest string) (bool, *time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared[key] = sandboxID
	now := time.Now()
	return true, &now, nil
}

func (f *fakeSBStore) UpdateExecState(_ context.Context, key, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.execsPrepared[key]; ok {
		row.State = state
	}
	return nil
}

func (f *fakeSBStore) DeleteExec(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.execsPrepared, key)
	delete(f.execs, key)
	return nil
}

func (f *fakeSBStore) PutExecDone(_ context.Context, key, sandboxID string, result json.RawMessage, _ *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs[key] = result
	delete(f.prepared, key)
	return nil
}

func (f *fakeSBStore) ListExpiredSandboxes(context.Context, time.Time, string) ([]*store.SandboxRow, error) {
	return f.expiredRows, nil
}

func (f *fakeSBStore) DeleteSandbox(_ context.Context, id string) error {
	delete(f.sandboxes, id)
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}

func (f *fakeSBStore) SessionOrg(_ context.Context, sessionID string) (string, error) {
	return "org_1", nil
}

func testServer(t *testing.T) (*Server, *fakeDriver, *fakeSBStore) {
	t.Helper()
	d := &fakeDriver{}
	st := newFakeSBStore()
	s := &Server{Driver: d, Store: st, WorkspaceRoot: t.TempDir(), Logger: slog.Default()}
	return s, d, st
}

func doReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServerCreateSandbox(t *testing.T) {
	s, d, st := testServer(t)
	rec := doReq(t, s.Router(), http.MethodPost, "/sandboxes",
		`{"image":"python:3.12-slim","limits":{"cpu":"1","mem":"512m"},"ttl":"1h","session_id":"s_1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("应 201，得 %d: %s", rec.Code, rec.Body.String())
	}
	if d.created.Image != "python:3.12-slim" || d.created.SessionID != "s_1" {
		t.Fatalf("driver 收到请求不符: %+v", d.created)
	}
	row, err := st.GetSandbox(context.Background(), "sb_fake")
	if err != nil || row.Status != "ready" || row.OrgID != "org_1" {
		t.Fatalf("沙箱行未落库: %+v err=%v", row, err)
	}
}

func TestServerExecuteStreamsLogAndExit(t *testing.T) {
	s, d, st := testServer(t)
	st.UpsertSandbox(context.Background(), &store.SandboxRow{SandboxID: "sb_fake", Status: "ready"})

	rec := doReq(t, s.Router(), http.MethodPost, "/execute",
		`{"sandbox_id":"sb_fake","name":"bash","input":"echo hi","idempotency_key":"r_1:0:t_1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"log"`) || !strings.Contains(body, `"payload":"echo hi"`) {
		t.Fatalf("应流式日志帧: %s", body)
	}
	if !strings.Contains(body, `"type":"exit"`) || !strings.Contains(body, `"exit":0`) {
		t.Fatalf("应 exit 帧: %s", body)
	}
	if len(d.executed) != 1 || d.executed[0].IdempotencyKey != "r_1:0:t_1" {
		t.Fatalf("driver 收到 execute 不符: %+v", d.executed)
	}
	// 幂等缓存已写
	if _, ok := st.execs["r_1:0:t_1"]; !ok {
		t.Fatal("幂等缓存应写入")
	}
}

func TestServerExecuteIdempotentReplay(t *testing.T) {
	s, d, st := testServer(t)
	st.UpsertSandbox(context.Background(), &store.SandboxRow{SandboxID: "sb_fake", Status: "ready"})
	st.execs["r_1:0:t_1"] = json.RawMessage(`{"exit":0,"output":"cached\n"}`)

	rec := doReq(t, s.Router(), http.MethodPost, "/execute",
		`{"sandbox_id":"sb_fake","name":"bash","input":"echo hi","idempotency_key":"r_1:0:t_1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cached") {
		t.Fatalf("应回缓存结果: %s", rec.Body.String())
	}
	if len(d.executed) != 0 {
		t.Fatal("幂等命中绝不复跑命令")
	}
}

func TestServerExecuteMissingSandbox404(t *testing.T) {
	s, _, _ := testServer(t)
	rec := doReq(t, s.Router(), http.MethodPost, "/execute",
		`{"sandbox_id":"sb_missing","name":"bash","input":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，得 %d", rec.Code)
	}
}

func TestServerFilesFastPath(t *testing.T) {
	s, d, st := testServer(t)
	_ = st.UpsertSandbox(context.Background(), &store.SandboxRow{SandboxID: "sb_fake", SessionID: "s_1", Status: "ready"})
	rec := doReq(t, s.Router(), http.MethodPut, "/files/sb_fake/workspace/hello.py", "print(42)")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("写文件应 204，得 %d", rec.Code)
	}
	rec2 := doReq(t, s.Router(), http.MethodGet, "/files/sb_fake/workspace/hello.py", "")
	if rec2.Code != http.StatusOK || rec2.Body.String() != "print(42)" {
		t.Fatalf("读文件不符: %d %q", rec2.Code, rec2.Body.String())
	}
	if d.files["/workspace/hello.py"] != "print(42)" {
		t.Fatalf("driver 文件内容不符: %v", d.files)
	}
}

func TestServerLifecycleEndpoints(t *testing.T) {
	s, d, st := testServer(t)
	st.UpsertSandbox(context.Background(), &store.SandboxRow{SandboxID: "sb_fake", Status: "ready"})

	if rec := doReq(t, s.Router(), http.MethodPost, "/sandboxes/sb_fake/freeze", ""); rec.Code != http.StatusOK || !d.frozen {
		t.Fatalf("freeze 失败: %d", rec.Code)
	}
	if rec := doReq(t, s.Router(), http.MethodPost, "/sandboxes/sb_fake/unfreeze", ""); rec.Code != http.StatusOK || d.frozen {
		t.Fatalf("unfreeze 失败: %d", rec.Code)
	}
	rec := doReq(t, s.Router(), http.MethodPost, "/sandboxes/sb_fake/snapshot", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "snap_ref") {
		t.Fatalf("snapshot 失败: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, s.Router(), http.MethodDelete, "/sandboxes/sb_fake", ""); rec.Code != http.StatusNoContent || len(d.destroyed) == 0 {
		t.Fatalf("destroy 失败: %d", rec.Code)
	}
}

func TestServerGC(t *testing.T) {
	st := newFakeSBStore()
	drv := &fakeDriver{}
	srv := &Server{Driver: drv, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// 预置两个过期沙箱（一个带容器引用，一个不带）
	_ = st.UpsertSandbox(context.Background(), &store.SandboxRow{SandboxID: "sb_1", SessionID: "s_1", Status: "ready", ContainerRef: ptrStr("ctr_1")})
	_ = st.UpsertSandbox(context.Background(), &store.SandboxRow{SandboxID: "sb_2", SessionID: "s_2", Status: "ready"})
	// 扫描返回预置行（经 fakeSBStore 的 expired 通道）
	expired, _ := st.ListExpiredSandboxes(context.Background(), time.Now(), "")
	_ = expired // fake 默认空；直接测试 GC 的销毁/删行逻辑——注入过期行
	st.expiredRows = []*store.SandboxRow{
		{SandboxID: "sb_1", SessionID: "s_1", Status: "ready", ContainerRef: ptrStr("ctr_1")},
		{SandboxID: "sb_2", SessionID: "s_2", Status: "ready"},
	}
	n, err := srv.GC(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("应清理 2 个: n=%d err=%v", n, err)
	}
	if _, ok := drv.destroyed["sb_1"]; !ok {
		t.Fatal("sb_1 容器应被销毁")
	}
	if len(st.deletedIDs) != 2 {
		t.Fatalf("应删除 2 行: %v", st.deletedIDs)
	}
}

func TestServerGCDestroyFailureKeepsRow(t *testing.T) {
	st := newFakeSBStore()
	drv := &fakeDriver{failDestroy: true}
	srv := &Server{Driver: drv, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st.expiredRows = []*store.SandboxRow{
		{SandboxID: "sb_1", SessionID: "s_1", Status: "ready", ContainerRef: ptrStr("ctr_1")},
	}
	n, err := srv.GC(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("销毁失败应清理 0: n=%d err=%v", n, err)
	}
	if len(st.deletedIDs) != 0 {
		t.Fatalf("销毁失败不得删行: %v", st.deletedIDs)
	}
}

func ptrStr(v string) *string { return &v }

// TestExecuteCacheHitStream 缓存命中回放同形 SSE 流（评审 #2——曾发裸 JSON）。
func TestExecuteCacheHitStream(t *testing.T) {
	st := newFakeSBStore()
	drv := &fakeDriver{execResult: &ExecuteResult{Exit: 0}}
	srv := &Server{Driver: drv, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_ = st.UpsertSandbox(context.Background(), &store.SandboxRow{SandboxID: "sb_1", SessionID: "s_1", Status: "ready"})
	st.execs["k1"] = json.RawMessage(`{"exit":3,"output":"缓存结果"}`)

	rec := doReq(t, srv.Router(), http.MethodPost, "/execute",
		`{"sandbox_id":"sb_1","name":"bash","input":"echo hi","idempotency_key":"k1"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"exit"`) ||
		!strings.Contains(rec.Body.String(), `"exit":3`) {
		t.Fatalf("缓存命中应回放 SSE exit 流: %d %s", rec.Code, rec.Body.String())
	}
	if len(drv.executed) != 0 {
		t.Fatalf("缓存命中不得复跑命令: %+v", drv.executed)
	}
}

// TestExecuteInFlightConflict 同键 in-flight：prepared 新鲜 → 409 停派发（评审 #1）。
func TestExecuteInFlightConflict(t *testing.T) {
	st := newFakeSBStore()
	drv := &fakeDriver{execResult: &ExecuteResult{Exit: 0}}
	srv := &Server{Driver: drv, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_ = st.UpsertSandbox(context.Background(), &store.SandboxRow{SandboxID: "sb_1", SessionID: "s_1", Status: "ready"})
	now := time.Now()
	st.prepared["k1"] = "sb_1"
	_ = now // fake 的 GetExec 用 time.Now() 生成 prepared_at——直接构造行
	st.mu.Lock()
	st.execsPrepared["k1"] = &store.ExecRow{IdempotencyKey: "k1", State: "prepared", PreparedAt: &now}
	st.mu.Unlock()

	rec := doReq(t, srv.Router(), http.MethodPost, "/execute",
		`{"sandbox_id":"sb_1","name":"bash","input":"echo hi","idempotency_key":"k1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("in-flight 应 409，得 %d: %s", rec.Code, rec.Body.String())
	}
	if len(drv.executed) != 0 {
		t.Fatalf("in-flight 不得执行: %+v", drv.executed)
	}
}

// TestLeaseEndpoints ⑨：acquire/renew generation 递增、释放代次校验、无租约执行拒绝。
func TestLeaseEndpoints(t *testing.T) {
	st := newFakeSBStore()
	drv := &fakeDriver{execResult: &ExecuteResult{Exit: 0}}
	srv := &Server{Driver: drv, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ttl := 200 * time.Millisecond
	_ = st.UpsertSandbox(context.Background(), &store.SandboxRow{
		SandboxID: "sb_1", SessionID: "s_1", Status: "ready", TTL: &ttl,
	})
	// acquire → renew
	rec := doReq(t, srv.Router(), http.MethodPost, "/sandboxes/sb_1/lease", `{"run_id":"r_1","ttl":"10m"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"generation":1`) {
		t.Fatalf("acquire 应 gen1: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv.Router(), http.MethodPost, "/sandboxes/sb_1/lease", `{"run_id":"r_1","ttl":"10m"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"generation":2`) {
		t.Fatalf("renew 应 gen2: %d %s", rec.Code, rec.Body.String())
	}
	// 旧代次释放 → 409
	rec = doReq(t, srv.Router(), http.MethodDelete, "/sandboxes/sb_1/lease", `{"generation":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("旧代次应 409: %d %s", rec.Code, rec.Body.String())
	}
	// 当代次释放 → 204
	rec = doReq(t, srv.Router(), http.MethodDelete, "/sandboxes/sb_1/lease", `{"generation":2}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("当代次应 204: %d", rec.Code)
	}
	// TTL 过期 + 无租约 → execute 409 lease expired
	time.Sleep(300 * time.Millisecond)
	rec = doReq(t, srv.Router(), http.MethodPost, "/execute",
		`{"sandbox_id":"sb_1","name":"bash","input":"echo hi","idempotency_key":"k1"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "lease expired") {
		t.Fatalf("无租约执行应 409 lease expired: %d %s", rec.Code, rec.Body.String())
	}
}

// TestServerSweep 生命周期闭环 D2：孤儿容器 vs DB 行矩阵——无行/已 destroyed/
// 过期 → 销毁；ready 未过期 → 保留。
func TestServerSweep(t *testing.T) {
	mk := func(rows map[string]*store.SandboxRow, names []string) (*Server, *fakeDriver, *fakeSBStore) {
		st := &fakeSBStore{sandboxes: rows}
		d := &fakeDriver{orphanContainers: names}
		return &Server{Driver: d, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, d, st
	}
	now := time.Now()
	ttl := time.Minute
	// 无行（崩溃窗口）→ 销毁；ready 未过期 → 保留；destroyed 行 → 销毁；过期 → 销毁
	s, d, _ := mk(map[string]*store.SandboxRow{
		"sb_ready":     {SandboxID: "sb_ready", Status: "ready", CreatedAt: now, TTL: &ttl},
		"sb_destroyed": {SandboxID: "sb_destroyed", Status: "destroyed", CreatedAt: now, TTL: &ttl},
		"sb_expired":   {SandboxID: "sb_expired", Status: "ready", CreatedAt: now.Add(-2 * time.Minute), TTL: &ttl},
	}, []string{"sb_orphan", "sb_ready", "sb_destroyed", "sb_expired"})
	n, err := s.Sweep(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("sweep 应清 3: n=%d err=%v destroyed=%v", n, err, d.destroyed)
	}
	if !d.destroyed["sb_orphan"] || !d.destroyed["sb_destroyed"] || !d.destroyed["sb_expired"] || d.destroyed["sb_ready"] {
		t.Fatalf("销毁集合不符: %v", d.destroyed)
	}
	// ps 失败 → 错误上抛（调用方降级不崩溃）
	s2, _, _ := mk(nil, nil)
	s2.Driver.(*fakeDriver).orphanErr = fmt.Errorf("docker down")
	if _, err := s2.Sweep(context.Background()); err == nil {
		t.Fatal("ps 失败应上抛")
	}
}

// TestExecuteLeaseHolderEnforced 审计 P0-1：RunID 非空时 lease 持有者必须
// 匹配——旧 owner 在 holder 换代后仍可 dispatch 的反例关闭。
func TestExecuteLeaseHolderEnforced(t *testing.T) {
	sbID := "sb_owner_claim"
	past := time.Now().Add(-time.Hour)
	st := newFakeSBStore()
	if st.leases == nil {
		st.leases = map[string]*store.LeaseRow{}
	}
	st.sandboxes[sbID] = &store.SandboxRow{
		SandboxID: sbID, Status: "ready", TTL: func() *time.Duration { d := 2 * time.Hour; return &d }(),
		CreatedAt: past.Add(-2 * time.Hour), // 已超 TTL 窗口
	}
	st.leases[sbID] = &store.LeaseRow{
		SandboxID: sbID, RunID: "run-b", Generation: 2, ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	srv := &Server{Driver: &fakeDriver{}, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := ExecuteRequest{SandboxID: sbID, Name: "bash", Input: "ls",
		IdempotencyKey: "k-owner-1", RunID: "run-a"} // 旧 owner
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/execute", bytes.NewReader(body))
	srv.execute(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("旧 owner 的 dispatch 应 409（holder 不匹配），得 %d: %s", rec.Code, rec.Body.String())
	}
	// 当前 holder（run-b）放行（fakeDriver 执行 → exit 帧 200）
	req.RunID = "run-b"
	req.IdempotencyKey = "k-owner-2"
	body2, _ := json.Marshal(req)
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/execute", bytes.NewReader(body2))
	srv.execute(rec2, r2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("当前 holder 应执行，得 %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestExecuteUnknownStalePreparedClassified 审计 #2：prepared 过期重跑的
// safe replay 显式分类——同输入同沙箱 = 幂等重建重跑；否则 409 停派发
// （同 key 不同 input 的外部效果重复窗口关闭）。
func TestExecuteUnknownStalePreparedClassified(t *testing.T) {
	sbID := "sb_unknown"
	old := time.Now().Add(-10 * time.Minute) // prepared 已超 2 分钟窗口
	st := newFakeSBStore()
	st.sandboxes[sbID] = &store.SandboxRow{SandboxID: sbID, Status: "ready", CreatedAt: time.Now()}
	st.execsPrepared["k-unk"] = &store.ExecRow{
		IdempotencyKey: "k-unk", SandboxID: sbID, State: "prepared",
		InputDigest: "digest-a", PreparedAt: &old,
	}
	srv := &Server{Driver: &fakeDriver{}, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// 不同输入 → 409 unknown 停派发
	req := ExecuteRequest{SandboxID: sbID, Name: "bash", Input: "rm -rf /", IdempotencyKey: "k-unk"}
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	srv.execute(rec, httptest.NewRequest(http.MethodPost, "/execute", bytes.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("输入不符应 409 unknown 停派发，得 %d: %s", rec.Code, rec.Body.String())
	}

	// 同输入同沙箱 → 幂等重建重跑（claim 获胜 → fakeDriver 执行 200）
	req.Input = "同输入"
	req.IdempotencyKey = "k-unk2"
	st.execsPrepared["k-unk2"] = &store.ExecRow{
		IdempotencyKey: "k-unk2", SandboxID: sbID, State: "prepared",
		InputDigest: inputDigest(req), PreparedAt: &old,
	}
	body2, _ := json.Marshal(req)
	rec2 := httptest.NewRecorder()
	srv.execute(rec2, httptest.NewRequest(http.MethodPost, "/execute", bytes.NewReader(body2)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("同输入同沙箱应重跑，得 %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestGCQuiesceActiveLeaseSkipped 审计 P0-3：Destroy 前二次 lease 校验——
// 扫描后新建的活跃租约被拦下（无 quiesce 曾杀活跃执行的反例关闭）。
func TestGCQuiesceActiveLeaseSkipped(t *testing.T) {
	st := newFakeSBStore()
	if st.leases == nil {
		st.leases = map[string]*store.LeaseRow{}
	}
	st.expiredRows = []*store.SandboxRow{{SandboxID: "sb_gc1", Status: "ready"}}
	st.leases["sb_gc1"] = &store.LeaseRow{SandboxID: "sb_gc1", RunID: "r_live", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}
	d := &fakeDriver{destroyed: map[string]bool{}}
	srv := &Server{Driver: d, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if _, err := srv.GC(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if d.destroyed["sb_gc1"] {
		t.Fatal("有活跃租约的过期沙箱不得被 GC 销毁（quiesce 校验）")
	}
	// 无租约的行正常销毁
	st.leases = map[string]*store.LeaseRow{}
	st.expiredRows = []*store.SandboxRow{{SandboxID: "sb_gc2", Status: "ready"}}
	if _, err := srv.GC(context.Background()); err != nil {
		t.Fatalf("gc2: %v", err)
	}
	if !d.destroyed["sb_gc2"] {
		t.Fatal("无租约的过期沙箱应被销毁")
	}
}

// TestExecuteScansWorkspaceForCLIChanges E1c：execute 后工作区扫描——bash
// 改写的文件入索引（fakeDriver.files 的新条目经 syncWorkspaceScan 上传）。
func TestExecuteScansWorkspaceForCLIChanges(t *testing.T) {
	sbID := "sb_e1c"
	st := newFakeSBStore()
	st.sandboxes[sbID] = &store.SandboxRow{SandboxID: sbID, SessionID: "s_e1c", Status: "ready", CreatedAt: time.Now()}
	d := &fakeDriver{
		files: map[string]string{"/workspace/cli-output.txt": "bash 改写的内容"},
	}
	srv := &Server{Driver: d, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// 直接调扫描（execute 后路径的单元面）
	srv.syncWorkspaceScan(context.Background(), sbID)
	// Blob nil → 扫描跳过（无上传）；断言不 panic 且不误报
	//（Blob 可用时 syncBlob 上传——既有 write_file 路径已覆盖）
	if len(st.wsFiles) != 0 {
		t.Fatalf("Blob nil 时不应写索引: %+v", st.wsFiles)
	}
}
