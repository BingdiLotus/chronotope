package execproto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// E2BDriver 实现 Driver（契约规范 §4 的 prod 档：E2B 沙箱，落地方案 §12）。
// 架构与 DockerDriver 同构：驱动逻辑纯函数化，wire 层经 E2BAPI 接口隔离——
// 单测用 fake API，HTTPE2BAPI 的请求形状用 mock server 验证，真实实例
// （含自托管 E2B_API_URL）用 env 门控 e2e 验证。

// E2BAPI 是 E2B API 的最小客户端面（driver 依赖，测试替身注入）。
type E2BAPI interface {
	CreateSandbox(ctx context.Context, templateID string, timeoutMs int64) (string, error)
	// StartProcess 官方云 ConnectRPC 命令执行（自托管实现返回不支持错误）
	StartProcess(ctx context.Context, sandboxID, cmd string) (stdout, stderr string, exitCode int, err error)
	// IsOfficial 官方云判定（驱动命令/文件的路由分支）
	IsOfficial() bool
	RunCommand(ctx context.Context, sandboxID, cmd, cwd string, timeoutMs int64) (stdout, stderr string, exitCode int, err error)
	ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error)
	WriteFile(ctx context.Context, sandboxID, path string, data []byte) error
	Pause(ctx context.Context, sandboxID string) error
	Resume(ctx context.Context, sandboxID string) error
	CreateSnapshot(ctx context.Context, sandboxID, snapshotID string) error
	Delete(ctx context.Context, sandboxID string) error
}

// E2BDriver 以镜像名作 templateID（部署方把镜像映射为 E2B 模板；
// E2B_TEMPLATE 环境变量可全局覆盖；E2B_TEMPLATE_MAP 按镜像名映射——
// 官方云无 python/golang 等模板，template 'python' not found 实证）。
type E2BDriver struct {
	API         E2BAPI
	Template    string
	TemplateMap map[string]string // image → 模板 ID（E2B_TEMPLATE_MAP JSON）
	Timeout     time.Duration     // 命令/创建默认超时（默认 5min）
}

func NewE2BDriver(api E2BAPI, template string) *E2BDriver {
	return &E2BDriver{API: api, Template: template, Timeout: 5 * time.Minute}
}

// resolveTemplate 镜像名 → 模板（映射表 → 全局 E2B_TEMPLATE → 镜像名原样）。
func (d *E2BDriver) resolveTemplate(image string) string {
	if d.TemplateMap != nil {
		if tpl, ok := d.TemplateMap[image]; ok && tpl != "" {
			return tpl
		}
		if tpl, ok := d.TemplateMap["*"]; ok && tpl != "" {
			return tpl
		}
	}
	if d.Template != "" {
		return d.Template
	}
	return image
}

// CreateSandbox → E2B 沙箱（id 即 sandbox id）。
func (d *E2BDriver) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	tpl := d.resolveTemplate(req.Image)
	id, err := d.API.CreateSandbox(ctx, tpl, d.Timeout.Milliseconds())
	if err != nil {
		return nil, err
	}
	return &Sandbox{ID: id, Driver: "e2b"}, nil
}

// Execute 命令执行：E2B 无流式通道，收集完成后写入 log（执行期间无帧；
// 长命令依赖 timeoutMs 上限——真实实例 smoke 验证）。
func (d *E2BDriver) Execute(ctx context.Context, req ExecuteRequest, log io.Writer) (*ExecuteResult, error) {
	var stdout, stderr string
	var exit int
	var err error
	if d.API.IsOfficial() {
		// 官方云：命令执行走 envd ConnectRPC（REST 无 commands 端点——诊断实证）
		stdout, stderr, exit, err = d.API.StartProcess(ctx, req.SandboxID, req.Input)
		if exit != 0 && err == nil {
			err = fmt.Errorf("e2b: process exit %d: %s%s", exit, stdout, truncateStr(stderr, 500))
		}
	} else {
		stdout, stderr, exit, err = d.API.RunCommand(ctx, req.SandboxID, req.Input, "", d.Timeout.Milliseconds())
	}
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(log, "%s%s", stdout, stderr)
	return &ExecuteResult{Exit: exit}, nil
}

func (d *E2BDriver) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	if !d.API.IsOfficial() {
		return d.API.ReadFile(ctx, sandboxID, path)
	}
	// 官方云：文件走命令（cat——文件 API 是 ConnectRPC filesystem 服务，后置）
	stdout, stderr, exit, err := d.API.StartProcess(ctx, sandboxID, "cat "+shellQuote(path))
	if err != nil || exit != 0 {
		return nil, fmt.Errorf("e2b: read file (exit %d): %s%s", exit, stderr, err)
	}
	return []byte(stdout), nil
}

func (d *E2BDriver) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	if !d.API.IsOfficial() {
		return d.API.WriteFile(ctx, sandboxID, path, data)
	}
	// 官方云：文件走命令（base64 管道防转义——heredoc 转义风险实证纪律）
	encoded := base64.StdEncoding.EncodeToString(data)
	cmd := "mkdir -p " + shellQuote(dirOf(path)) + " && echo " + encoded + " | base64 -d > " + shellQuote(path)
	_, stderr, exit, err := d.API.StartProcess(ctx, sandboxID, cmd)
	if err != nil || exit != 0 {
		return fmt.Errorf("e2b: write file (exit %d): %s%s", exit, stderr, err)
	}
	return nil
}

// Freeze/Unfreeze → E2B pause/resume（Tier 1 冻结：暂停计费与 CPU）。
func (d *E2BDriver) Freeze(ctx context.Context, sandboxID string) error {
	return d.API.Pause(ctx, sandboxID)
}
func (d *E2BDriver) Unfreeze(ctx context.Context, sandboxID string) error {
	return d.API.Resume(ctx, sandboxID)
}

// Snapshot → E2B 快照（Tier 2；ref = 快照 id，恢复时作模板重建）。
func (d *E2BDriver) Snapshot(ctx context.Context, sandboxID string) (string, error) {
	ref := "snap_" + sandboxID + "_" + time.Now().UTC().Format("20060102150405")
	if err := d.API.CreateSnapshot(ctx, sandboxID, ref); err != nil {
		return "", err
	}
	return ref, nil
}

// Destroy → E2B 删除（Tier 3）。
// ListOrphanContainers：E2B 沙箱生命周期由 E2B 侧 TTL 自动回收（微 VM 无本地
// 容器残留面）——sweep 空操作；容器卷泄漏是 docker driver 专属问题。
func (d *E2BDriver) ListOrphanContainers(context.Context) ([]string, error) {
	return nil, nil
}

func (d *E2BDriver) Destroy(ctx context.Context, sandboxID string) error {
	return d.API.Delete(ctx, sandboxID)
}

// HTTPE2BAPI 是 E2B API v2 的 HTTP 客户端（X-API-Key；base 默认
// https://api.e2b.dev，自托管设 E2B_API_URL——同协议同形状）。
type HTTPE2BAPI struct {
	Base string
	Key  string
	HTTP *http.Client
}

func NewHTTPE2BAPI(base, key string) *HTTPE2BAPI {
	if base == "" {
		base = "https://api.e2b.dev"
	}
	return &HTTPE2BAPI{Base: strings.TrimRight(base, "/"), Key: key, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

func (c *HTTPE2BAPI) do(ctx context.Context, method, path string, body []byte, out any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("e2b: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("e2b: %s %s: status %d: %s", method, path, resp.StatusCode, truncateStr(string(raw), 300))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("e2b: decode %s: %w", path, err)
		}
	}
	return raw, nil
}

func (c *HTTPE2BAPI) CreateSandbox(ctx context.Context, templateID string, timeoutMs int64) (string, error) {
	body, _ := json.Marshal(map[string]any{"templateID": templateID, "timeoutMs": timeoutMs})
	var out struct {
		SandboxID string `json:"sandboxID"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/v2/sandboxes", body, &out); err != nil {
		return "", err
	}
	if out.SandboxID == "" {
		return "", fmt.Errorf("e2b: create sandbox: 响应缺 sandboxID")
	}
	return out.SandboxID, nil
}

func (c *HTTPE2BAPI) RunCommand(ctx context.Context, sandboxID, cmd, cwd string, timeoutMs int64) (string, string, int, error) {
	body, _ := json.Marshal(map[string]any{"cmd": cmd, "cwd": cwd, "timeoutMs": timeoutMs})
	var out struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exitCode"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/v2/sandboxes/"+url.PathEscape(sandboxID)+"/commands", body, &out); err != nil {
		return "", "", 0, err
	}
	return out.Stdout, out.Stderr, out.ExitCode, nil
}

func (c *HTTPE2BAPI) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	raw, err := c.do(ctx, http.MethodGet, "/v2/sandboxes/"+url.PathEscape(sandboxID)+"/files?path="+url.QueryEscape(path), nil, nil)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *HTTPE2BAPI) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	_, err := c.do(ctx, http.MethodPost, "/v2/sandboxes/"+url.PathEscape(sandboxID)+"/files?path="+url.QueryEscape(path), data, nil)
	return err
}

func (c *HTTPE2BAPI) Pause(ctx context.Context, sandboxID string) error {
	_, err := c.do(ctx, http.MethodPost, "/v2/sandboxes/"+url.PathEscape(sandboxID)+"/pause", []byte("{}"), nil)
	return err
}

func (c *HTTPE2BAPI) Resume(ctx context.Context, sandboxID string) error {
	_, err := c.do(ctx, http.MethodPost, "/v2/sandboxes/"+url.PathEscape(sandboxID)+"/resume", []byte("{}"), nil)
	return err
}

func (c *HTTPE2BAPI) CreateSnapshot(ctx context.Context, sandboxID, snapshotID string) error {
	body, _ := json.Marshal(map[string]any{"snapshotId": snapshotID})
	_, err := c.do(ctx, http.MethodPost, "/v2/sandboxes/"+url.PathEscape(sandboxID)+"/snapshots", body, nil)
	return err
}

func (c *HTTPE2BAPI) Delete(ctx context.Context, sandboxID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v2/sandboxes/"+url.PathEscape(sandboxID), nil, nil)
	return err
}

// IsOfficial 官方云判定（无自托管 E2B_API_URL——Base 为官方平台域）。
func (c *HTTPE2BAPI) IsOfficial() bool {
	return strings.Contains(c.Base, "api.e2b") || c.Base == ""
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
