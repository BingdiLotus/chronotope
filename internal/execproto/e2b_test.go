package execproto

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeE2BAPI 是 driver 逻辑测试替身。
type fakeE2BAPI struct {
	created    []string
	commands   []string
	files      map[string]string
	paused     bool
	snapshotID string
	deleted    string
	runOut     [3]any // stdout, stderr, exit
	runErr     error
	createErr  error
}

func (f *fakeE2BAPI) StartProcess(context.Context, string, string) (string, string, int, error) {
	return "", "", 0, errors.New("not official")
}

func (f *fakeE2BAPI) IsOfficial() bool { return false }

func (f *fakeE2BAPI) CreateSandbox(_ context.Context, templateID string, _ int64) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, templateID)
	return "e2b_sb_1", nil
}

func (f *fakeE2BAPI) RunCommand(_ context.Context, _ string, cmd, _ string, _ int64) (string, string, int, error) {
	if f.runErr != nil {
		return "", "", 0, f.runErr
	}
	f.commands = append(f.commands, cmd)
	return f.runOut[0].(string), f.runOut[1].(string), f.runOut[2].(int), nil
}

func (f *fakeE2BAPI) ReadFile(_ context.Context, _, path string) ([]byte, error) {
	return []byte(f.files[path]), nil
}

func (f *fakeE2BAPI) WriteFile(_ context.Context, _, path string, data []byte) error {
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}

func (f *fakeE2BAPI) Pause(context.Context, string) error {
	f.paused = true
	return nil
}

func (f *fakeE2BAPI) Resume(context.Context, string) error {
	f.paused = false
	return nil
}

func (f *fakeE2BAPI) CreateSnapshot(_ context.Context, _, snapshotID string) error {
	f.snapshotID = snapshotID
	return nil
}

func (f *fakeE2BAPI) Delete(_ context.Context, id string) error {
	f.deleted = id
	return nil
}

// TestE2BDriverLifecycle 全生命周期：创建（镜像→模板映射）→ 执行（日志写入）
// → 文件读写 → 冻结/解冻 → 快照 → 销毁。
func TestE2BDriverLifecycle(t *testing.T) {
	fake := &fakeE2BAPI{files: map[string]string{}, runOut: [3]any{"hello", "", 0}}
	d := NewE2BDriver(fake, "")

	sb, err := d.CreateSandbox(context.Background(), CreateSandboxRequest{Image: "python:3.11"})
	if err != nil || sb.ID != "e2b_sb_1" {
		t.Fatalf("create: %+v err=%v", sb, err)
	}
	if len(fake.created) != 1 || fake.created[0] != "python:3.11" {
		t.Fatalf("镜像应映射为模板: %v", fake.created)
	}
	var log strings.Builder
	res, err := d.Execute(context.Background(), ExecuteRequest{SandboxID: "e2b_sb_1", Input: "echo hi"}, &log)
	if err != nil || res.Exit != 0 || !strings.Contains(log.String(), "hello") {
		t.Fatalf("execute: %+v err=%v log=%q", res, err, log.String())
	}
	if err := d.WriteFile(context.Background(), "e2b_sb_1", "/workspace/a.txt", []byte("内容")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := d.ReadFile(context.Background(), "e2b_sb_1", "/workspace/a.txt")
	if err != nil || string(got) != "内容" {
		t.Fatalf("read: %q err=%v", got, err)
	}
	if err := d.Freeze(context.Background(), "e2b_sb_1"); err != nil || !fake.paused {
		t.Fatalf("freeze: paused=%v err=%v", fake.paused, err)
	}
	if err := d.Unfreeze(context.Background(), "e2b_sb_1"); err != nil || fake.paused {
		t.Fatalf("unfreeze: paused=%v err=%v", fake.paused, err)
	}
	ref, err := d.Snapshot(context.Background(), "e2b_sb_1")
	if err != nil || ref == "" || fake.snapshotID != ref {
		t.Fatalf("snapshot: ref=%q err=%v", ref, err)
	}
	if err := d.Destroy(context.Background(), "e2b_sb_1"); err != nil || fake.deleted != "e2b_sb_1" {
		t.Fatalf("destroy: %v err=%v", fake.deleted, err)
	}
}

// TestE2BDriverTemplateOverride 显式模板优先于镜像名。
func TestE2BDriverTemplateOverride(t *testing.T) {
	fake := &fakeE2BAPI{files: map[string]string{}}
	d := NewE2BDriver(fake, "tpl-prod")
	_, err := d.CreateSandbox(context.Background(), CreateSandboxRequest{Image: "any"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fake.created[0] != "tpl-prod" {
		t.Fatalf("应使用显式模板: %v", fake.created)
	}
}

// TestHTTPE2BAPIWire 客户端 wire 形状（mock E2B 服务器校验路径/头/体）。
func TestHTTPE2BAPIWire(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("X-API-Key"), r.Method
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/sandboxes":
			_, _ = w.Write([]byte(`{"sandboxID":"sb_1"}`))
		case strings.HasSuffix(r.URL.Path, "/commands"):
			_, _ = w.Write([]byte(`{"stdout":"ok","stderr":"","exitCode":0}`))
		case strings.HasSuffix(r.URL.Path, "/files"):
			_, _ = w.Write([]byte("文件内容"))
		case strings.HasSuffix(r.URL.Path, "/snapshots"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := NewHTTPE2BAPI(srv.URL, "e2b_key_1")
	id, err := c.CreateSandbox(context.Background(), "tpl", 300000)
	if err != nil || id != "sb_1" || gotPath != "/v2/sandboxes" || gotAuth != "e2b_key_1" {
		t.Fatalf("create: id=%q err=%v path=%q auth=%q", id, err, gotPath, gotAuth)
	}
	if _, _, _, err := c.RunCommand(context.Background(), "sb_1", "echo hi", "", 1000); err != nil || gotPath != "/v2/sandboxes/sb_1/commands" {
		t.Fatalf("command: err=%v path=%q", err, gotPath)
	}
	raw, err := c.ReadFile(context.Background(), "sb_1", "/workspace/a.txt")
	if err != nil || string(raw) != "文件内容" || !strings.HasPrefix(gotPath, "/v2/sandboxes/sb_1/files") {
		t.Fatalf("read: %q err=%v path=%q", raw, err, gotPath)
	}
	if err := c.WriteFile(context.Background(), "sb_1", "/workspace/b.txt", []byte("x")); err != nil || gotMethod != http.MethodPost {
		t.Fatalf("write: err=%v method=%q", err, gotMethod)
	}
	if err := c.Pause(context.Background(), "sb_1"); err != nil || gotPath != "/v2/sandboxes/sb_1/pause" {
		t.Fatalf("pause: err=%v path=%q", err, gotPath)
	}
	if err := c.CreateSnapshot(context.Background(), "sb_1", "snap_1"); err != nil || gotPath != "/v2/sandboxes/sb_1/snapshots" {
		t.Fatalf("snapshot: err=%v path=%q", err, gotPath)
	}
	if err := c.Delete(context.Background(), "sb_1"); err != nil || gotMethod != http.MethodDelete {
		t.Fatalf("delete: err=%v method=%q", err, gotMethod)
	}
}

// TestHTTPE2BAPIError 非 2xx 映射为错误（含响应摘要）。
func TestHTTPE2BAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer srv.Close()
	c := NewHTTPE2BAPI(srv.URL, "bad")
	if _, err := c.CreateSandbox(context.Background(), "tpl", 1000); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("应报 401 错误: %v", err)
	}
}
