package execproto

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// CommandRunner 是 docker CLI 的薄封装（单测注入 fake；生产为 os/exec 实现）。
type CommandRunner interface {
	// Run 执行命令并返回输出；退出码非 0 时返回 *exec.ExitError。
	Run(ctx context.Context, args ...string) ([]byte, error)
	// Stream 执行命令并把 stdout/stderr 合并写入 out；退出码非 0 时返回 *exec.ExitError。
	Stream(ctx context.Context, out io.Writer, args ...string) error
}

// ExecCommandRunner 是生产实现（依赖宿主机 docker 守护进程：compose 已挂 docker.sock）。
type ExecCommandRunner struct{}

func (ExecCommandRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

func (ExecCommandRunner) Stream(ctx context.Context, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

var safeLimit = regexp.MustCompile(`^[0-9]+[bkmgBKMG]?i?$`)

// DockerDriver 是 dev 档沙箱实现（受限容器，mvp-落地方案 §2.4）：
// read-only root、tmpfs /tmp、--network none 默认（capabilities.network=true 时 bridge）、
// CPU/内存限额、无 secrets、会话作用域工作区卷。
// prod 档由 E2B driver 替换（同一 Driver 接口，capability 路由）。
type DockerDriver struct {
	Runner        CommandRunner
	WorkspaceRoot string // 宿主机工作区根（docker cp 文件快路径的暂存）
	// restoreTar/restoreID 暂存待恢复的卷 tar（CreateSandbox 创建后解回；单请求内）。
	restoreTar string
	restoreID  string
}

// NewDockerDriver 构造 docker driver（workspaceRoot 为空时用系统临时目录）。
func NewDockerDriver(runner CommandRunner, workspaceRoot string) *DockerDriver {
	if runner == nil {
		runner = ExecCommandRunner{}
	}
	if workspaceRoot == "" {
		workspaceRoot = filepath.Join(os.TempDir(), "chronotope-workspaces")
	}
	return &DockerDriver{Runner: runner, WorkspaceRoot: workspaceRoot}
}

func (d *DockerDriver) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	id := "sb_" + randHex(8)
	mem, cpu := limits(req.Limits)
	network := "none"
	if req.Capabilities.Network {
		network = "bridge" // MVP 简化：none|bridge 二档；精细 egress 白名单后置
	}
	// read-only root + /workspace 会话作用域卷 + tmpfs /tmp；无 secrets 注入（三条纪律之四）
	args := []string{"run", "-d", "--name", id,
		"--read-only",
		"--tmpfs", "/tmp:rw,size=64m",
		"--network", network,
		"--memory", mem,
		"--cpus", cpu,
		"--label", "chronotope.sandbox=" + id,
		"-e", "SANDBOX_ID=" + id,
		"-v", id + ":/workspace",
	}
	if req.RestoreFrom != "" {
		// Tier2 快照恢复：镜像 + 卷内容（评审 #7——卷不进镜像，显式解回）
		img, tarPath, found := strings.Cut(req.RestoreFrom, "|")
		if !found {
			img = req.RestoreFrom // 兼容旧 ref（仅镜像）
		}
		args = append(args, "--entrypoint", "sleep")
		args = append(args, img, "infinity")
		d.restoreTar = tarPath
		d.restoreID = id
	} else {
		args = append(args, req.Image, "sleep", "infinity")
	}
	out, err := d.Runner.Run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("docker create sandbox: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// 快照恢复：卷内容解回（评审 #7——named volume 不进镜像，必须显式恢复）
	if d.restoreID == id && d.restoreTar != "" {
		if out, err := d.Runner.Run(ctx, "exec", id, "mkdir", "-p", "/workspace"); err != nil {
			return nil, fmt.Errorf("restore mkdir: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := d.Runner.Run(ctx, "cp", d.restoreTar, id+":/workspace/restore.tar"); err != nil {
			return nil, fmt.Errorf("restore cp: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := d.Runner.Run(ctx, "exec", id, "tar", "-xf", "/workspace/restore.tar", "-C", "/workspace"); err != nil {
			return nil, fmt.Errorf("restore untar: %w: %s", err, strings.TrimSpace(string(out)))
		}
		d.restoreID, d.restoreTar = "", ""
	}
	return &Sandbox{ID: id, Status: "creating", Image: req.Image, Driver: "docker"}, nil
}

// limits 解析限额（契约允许 "512Mi"/"1"；空值走默认）。
func limits(l Limits) (mem, cpu string) {
	mem, cpu = "512m", "1"
	if safeLimit.MatchString(l.Mem) {
		mem = l.Mem
	}
	if safeLimit.MatchString(l.CPU) {
		cpu = l.CPU
	}
	return mem, cpu
}

// Execute 在沙箱内执行命令（name 为词汇表规范名），流式输出到 log（不含结果）。
func (d *DockerDriver) Execute(ctx context.Context, req ExecuteRequest, log io.Writer) (*ExecuteResult, error) {
	if err := d.checkSandbox(ctx, req.SandboxID); err != nil {
		return nil, err
	}
	args, err := execArgs(req.Name, req.Input)
	if err != nil {
		return nil, err
	}
	full := append([]string{"exec", req.SandboxID}, args...)
	err = d.Runner.Stream(ctx, log, full...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExecuteResult{Exit: exitErr.ExitCode()}, nil // 非零退出是正常结果，不是协议错误
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("docker exec: %w", ctx.Err())
		}
		return nil, fmt.Errorf("docker exec: %w", err)
	}
	return &ExecuteResult{Exit: 0}, nil
}

// execArgs 把词汇表工具名映射为容器内命令（契约规范 §3 词汇表）。
func execArgs(name, input string) ([]string, error) {
	switch name {
	case "bash", "list_files":
		if name == "list_files" {
			input = "ls -la " + input
		}
		return []string{"bash", "-c", input}, nil
	case "run_python":
		return []string{"python3", "-c", input}, nil
	default:
		return nil, fmt.Errorf("execproto: 工具 %q 不属代码执行类（词汇表约束）", name)
	}
}

// ReadFile 文件快路径（docker cp，不经过 shell）。
func (d *DockerDriver) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	if err := d.checkSandbox(ctx, sandboxID); err != nil {
		return nil, err
	}
	if err := d.prepareHostDir(sandboxID); err != nil {
		return nil, err
	}
	tmp := filepath.Join(d.hostDir(sandboxID), "read.out")
	if out, err := d.Runner.Run(ctx, "cp", sandboxID+":"+path, tmp); err != nil {
		return nil, fmt.Errorf("docker cp out: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(tmp)
}

// WriteFile 文件快路径（docker cp，不经过 shell）。
func (d *DockerDriver) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	if err := d.checkSandbox(ctx, sandboxID); err != nil {
		return err
	}
	if err := d.prepareHostDir(sandboxID); err != nil {
		return err
	}
	tmp := filepath.Join(d.hostDir(sandboxID), "write.in")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write staging: %w", err)
	}
	// 目标父目录缺失时 docker cp 报 "Could not find the file ... in container"
	//（skill 安装写 skills/<name>/SKILL.md，目录首次不存在——w6 e2e 实证）
	if dir := filepath.Dir(path); dir != "/" && dir != "." {
		if out, err := d.Runner.Run(ctx, "exec", sandboxID, "mkdir", "-p", dir); err != nil {
			return fmt.Errorf("docker exec mkdir: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := d.Runner.Run(ctx, "cp", tmp, sandboxID+":"+path); err != nil {
		return fmt.Errorf("docker cp in: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *DockerDriver) Freeze(ctx context.Context, sandboxID string) error {
	return d.simple(ctx, "freeze", "pause", sandboxID)
}

func (d *DockerDriver) Unfreeze(ctx context.Context, sandboxID string) error {
	return d.simple(ctx, "unfreeze", "unpause", sandboxID)
}

// Snapshot 是 Tier2 快照（docker commit → snapshot_ref；W2 实现，恢复路径后置）。
// Snapshot Tier 2 快照：docker commit 镜像 + /workspace 卷内容 tar 到宿主工作区。
// 评审 #7：named volume 不进 commit——必须显式打包卷内容，ref 编码
// "镜像|卷 tar 路径"（恢复时拆解）。
func (d *DockerDriver) Snapshot(ctx context.Context, sandboxID string) (string, error) {
	img := "chronotope-snap:" + sandboxID
	if out, err := d.Runner.Run(ctx, "commit", sandboxID, img); err != nil {
		return "", fmt.Errorf("docker commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// 卷内容打包（含 externalize 大输出——它们在 WorkspaceRoot 下同目录）
	if err := d.prepareHostDir(sandboxID); err != nil {
		return "", err
	}
	tarPath := filepath.Join(d.WorkspaceRoot, sandboxID, "volume.tar")
	if out, err := d.Runner.Run(ctx, "run", "--rm",
		"--volumes-from", sandboxID,
		"-v", filepath.Dir(tarPath)+":/backup",
		"alpine:3.20", "tar", "-cf", "/backup/volume.tar", "-C", "/workspace", "."); err != nil {
		return "", fmt.Errorf("snapshot volume tar: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return img + "|" + tarPath, nil
}

func (d *DockerDriver) Destroy(ctx context.Context, sandboxID string) error {
	return d.simple(ctx, "destroy", "rm", "-f", sandboxID)
}

func (d *DockerDriver) simple(ctx context.Context, op string, args ...string) error {
	if out, err := d.Runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("docker %s: %w: %s", op, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// checkSandbox 沙箱存在性检查（docker inspect；防对已销毁容器操作）。
func (d *DockerDriver) checkSandbox(ctx context.Context, sandboxID string) error {
	if _, err := d.Runner.Run(ctx, "inspect", sandboxID); err != nil {
		// 归一为 ErrSandboxNotFound 哨兵（server 层 404 → worker 恢复重建）
		return fmt.Errorf("sandbox %s 不存在或已销毁: %w: %w", sandboxID, ErrSandboxNotFound, err)
	}
	return nil
}

func (d *DockerDriver) hostDir(sandboxID string) string {
	return filepath.Join(d.WorkspaceRoot, sandboxID)
}

func (d *DockerDriver) prepareHostDir(sandboxID string) error {
	return os.MkdirAll(d.hostDir(sandboxID), 0o755)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
