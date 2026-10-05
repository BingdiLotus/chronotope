package execproto

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bingdilotus/chronotope/internal/store"
)

// fakeDriver 实现 Driver（服务器单测替身）。
type fakeDriver struct {
	created    CreateSandboxRequest
	sandboxID  string
	executed   []ExecuteRequest
	execResult *ExecuteResult
	files      map[string]string
	frozen     bool
	destroyed  bool
	snapshot   string
}

func (f *fakeDriver) CreateSandbox(_ context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	f.created = req
	f.sandboxID = "sb_fake"
	return &Sandbox{ID: f.sandboxID, Status: "creating", Image: req.Image, Driver: "docker"}, nil
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
func (f *fakeDriver) Destroy(context.Context, string) error { f.destroyed = true; return nil }

// fakeSBStore 实现 SandboxStore（服务器单测替身）。
type fakeSBStore struct {
	sandboxes map[string]*store.SandboxRow
	execs     map[string]json.RawMessage
}

func newFakeSBStore() *fakeSBStore {
	return &fakeSBStore{sandboxes: map[string]*store.SandboxRow{}, execs: map[string]json.RawMessage{}}
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
	if raw, ok := f.execs[key]; ok {
		return &store.ExecRow{IdempotencyKey: key, Result: raw}, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeSBStore) PutExec(_ context.Context, key, sandboxID string, result json.RawMessage) error {
	f.execs[key] = result
	return nil
}
func (f *fakeSBStore) SessionOrg(_ context.Context, sessionID string) (string, error) {
	return "org_1", nil
}

func testServer(t *testing.T) (*Server, *fakeDriver, *fakeSBStore) {
	t.Helper()
	d := &fakeDriver{}
	st := newFakeSBStore()
	s := &Server{Driver: d, Store: st, WorkspaceRoot: t.TempDir()}
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
	s, d, _ := testServer(t)
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
	if rec := doReq(t, s.Router(), http.MethodDelete, "/sandboxes/sb_fake", ""); rec.Code != http.StatusNoContent || !d.destroyed {
		t.Fatalf("destroy 失败: %d", rec.Code)
	}
}
