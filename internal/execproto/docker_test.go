package execproto

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeRunner 记录命令并按脚本回放。
type fakeRunner struct {
	calls   [][]string
	results map[string]string // 命令特征 → 输出
	streams map[string]string
	exitErr map[string]int
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		results: map[string]string{},
		streams: map[string]string{},
		exitErr: map[string]int{},
	}
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	key := strings.Join(args, " ")
	if _, ok := f.exitErr[key]; ok {
		return []byte(f.results[key]), exitError(1)
	}
	// docker cp 出容器（src 含 ':'）→ 模拟落盘，供 ReadFile 回读
	if len(args) == 3 && args[0] == "cp" && strings.Contains(args[1], ":") {
		_ = os.WriteFile(args[2], []byte(f.results[key]), 0o644)
	}
	return []byte(f.results[key]), nil
}

func (f *fakeRunner) Stream(_ context.Context, out io.Writer, args ...string) error {
	f.calls = append(f.calls, args)
	key := strings.Join(args, " ")
	_, _ = io.WriteString(out, f.streams[key])
	if code, ok := f.exitErr[key]; ok {
		return exitError(code)
	}
	return nil
}

// exitError 构造真实的 *exec.ExitError（os.ProcessState 不可直接构造）。
func exitError(code int) *exec.ExitError {
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code))
	err := cmd.Run()
	if e, ok := err.(*exec.ExitError); ok {
		return e
	}
	panic(fmt.Sprintf("构造 exitError 失败: %v", err))
}

func TestDockerDriverCreateSandboxArgs(t *testing.T) {
	r := newFakeRunner()
	d := NewDockerDriver(r, t.TempDir())

	sb, err := d.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image:  "python:3.12-slim",
		Limits: Limits{CPU: "2", Mem: "1Gi"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.ID == "" || sb.Driver != "docker" {
		t.Fatalf("沙箱引用不符: %+v", sb)
	}
	if len(r.calls) != 1 {
		t.Fatalf("应执行 1 条命令，得 %d", len(r.calls))
	}
	args := strings.Join(r.calls[0], " ")
	for _, want := range []string{"--read-only", "--network", "none", "--memory", "1Gi", "--cpus", "2", "sleep", "infinity"} {
		if !strings.Contains(args, want) {
			t.Fatalf("创建参数缺 %q: %s", want, args)
		}
	}
	if !strings.Contains(args, "python:3.12-slim") {
		t.Fatalf("镜像缺失: %s", args)
	}

	// network=true → bridge
	r2 := newFakeRunner()
	d2 := NewDockerDriver(r2, t.TempDir())
	if _, err := d2.CreateSandbox(context.Background(), CreateSandboxRequest{
		Image: "x", Capabilities: Capabilities{Network: true},
	}); err != nil {
		t.Fatalf("create with network: %v", err)
	}
	if got := strings.Join(r2.calls[0], " "); !strings.Contains(got, "--network bridge") {
		t.Fatalf("network=true 应 bridge: %s", got)
	}
}

func TestDockerDriverExecuteNameMapping(t *testing.T) {
	r := newFakeRunner()
	r.streams["exec sb_1 bash -c pytest"] = "collected 3 items\n3 passed\n"
	d := NewDockerDriver(r, t.TempDir())

	var log strings.Builder
	res, err := d.Execute(context.Background(), ExecuteRequest{
		SandboxID: "sb_1", Name: "bash", Input: "pytest",
	}, &log)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Exit != 0 || !strings.Contains(log.String(), "3 passed") {
		t.Fatalf("结果不符: %+v log=%q", res, log.String())
	}

	// run_python → python3 -c
	r2 := newFakeRunner()
	r2.streams["exec sb_1 python3 -c print(42)"] = "42\n"
	d2 := NewDockerDriver(r2, t.TempDir())
	if _, err := d2.Execute(context.Background(), ExecuteRequest{
		SandboxID: "sb_1", Name: "run_python", Input: "print(42)",
	}, io.Discard); err != nil {
		t.Fatalf("run_python: %v", err)
	}

	// 非零退出是正常结果
	r3 := newFakeRunner()
	r3.exitErr["exec sb_1 bash -c exit 2"] = 2
	d3 := NewDockerDriver(r3, t.TempDir())
	res3, err := d3.Execute(context.Background(), ExecuteRequest{
		SandboxID: "sb_1", Name: "bash", Input: "exit 2",
	}, io.Discard)
	if err != nil || res3.Exit != 2 {
		t.Fatalf("非零退出应返回 exit code: %+v err=%v", res3, err)
	}

	// 非执行类工具拒绝
	if _, err := d3.Execute(context.Background(), ExecuteRequest{
		SandboxID: "sb_1", Name: "web_search", Input: "x",
	}, io.Discard); err == nil {
		t.Fatal("非代码工具应拒绝")
	}
}

func TestDockerDriverFilesRoundtrip(t *testing.T) {
	r := newFakeRunner()
	r.results["cp sb_1:/workspace/hello.py /tmp/chronotope-workspaces/sb_1/read.out"] = "sent"
	d := NewDockerDriver(r, t.TempDir())

	if err := d.WriteFile(context.Background(), "sb_1", "/workspace/hello.py", []byte("print(42)")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := d.ReadFile(context.Background(), "sb_1", "/workspace/hello.py"); err != nil {
		t.Fatalf("read: %v", err)
	}
	// 文件快路径：docker cp（不经 shell）——一次入、一次出
	cpCalls := 0
	for _, call := range r.calls {
		if len(call) >= 2 && call[0] == "cp" && strings.Contains(strings.Join(call, " "), "/workspace/hello.py") {
			cpCalls++
		}
	}
	if cpCalls != 2 {
		t.Fatalf("读写应各一次 docker cp，得 %d: %v", cpCalls, r.calls)
	}
}

func TestDockerDriverLifecycle(t *testing.T) {
	r := newFakeRunner()
	d := NewDockerDriver(r, t.TempDir())
	ctx := context.Background()

	if err := d.Freeze(ctx, "sb_1"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if err := d.Unfreeze(ctx, "sb_1"); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	ref, err := d.Snapshot(ctx, "sb_1")
	if err != nil || ref == "" {
		t.Fatalf("snapshot: %v %q", err, ref)
	}
	if err := d.Destroy(ctx, "sb_1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	joined := []string{}
	for _, call := range r.calls {
		joined = append(joined, strings.Join(call, " "))
	}
	all := strings.Join(joined, " | ")
	for _, want := range []string{"pause sb_1", "unpause sb_1", "commit sb_1", "rm -f sb_1"} {
		if !strings.Contains(all, want) {
			t.Fatalf("生命周期缺 %q: %s", want, all)
		}
	}
}

func TestDockerDriverMissingSandboxErrors(t *testing.T) {
	r := newFakeRunner()
	r.exitErr["inspect sb_gone"] = 1
	d := NewDockerDriver(r, t.TempDir())
	if _, err := d.Execute(context.Background(), ExecuteRequest{
		SandboxID: "sb_gone", Name: "bash", Input: "ls",
	}, io.Discard); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失沙箱应报错: %v", err)
	}
}
