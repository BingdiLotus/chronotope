package restate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bingdilotus/chronotope/internal/execproto"
)

// Executor 是 execute 协议客户端的最小接口（worker-架构设计 §1 EC；
// 代码/命令类工具一律经 executor，harness 从不直连）。
type Executor interface {
	CreateSandbox(ctx context.Context, req execproto.CreateSandboxRequest) (string, error)
	Execute(ctx context.Context, sandboxID, name, input, idempotencyKey string) (*ExecResult, error)
	ReadFile(ctx context.Context, sandboxID, path string) (string, error)
	WriteFile(ctx context.Context, sandboxID, path, content string) error
	// ComputeLease（正确性二期 ⑨）：续约返回代次（终态释放用）；释放幂等。
	AcquireLease(ctx context.Context, sandboxID, runID string, ttl string) (int64, error)
	ReleaseLease(ctx context.Context, sandboxID string, generation int64) error
	// Snapshot 沙箱快照（时间旅行 期 2：checkpoint 的空间面；无沙箱返回空）
	Snapshot(ctx context.Context, sandboxID string) (string, error)
}

// ExecResult 是 execute 的聚合结果（输出回喂模型的载体）。
type ExecResult struct {
	Exit      int    `json:"exit"`
	Output    string `json:"output,omitempty"`     // 内联输出（≤256KB）
	OutputRef string `json:"output_ref,omitempty"` // 外置引用
	Truncated bool   `json:"truncated,omitempty"`  // 输出被外置截断
}

// executorClient 是 HTTP 实现（流式日志帧被消费为完整输出）。
type executorClient struct {
	baseURL string
	client  *http.Client
}

// NewExecutorClient 构造 executor 客户端（baseURL 形如 http://executor:9082）。
func NewExecutorClient(baseURL string) Executor {
	// 禁用连接复用：executor 可重启（容器 IP 会变），复用死连接导致瞬时 EOF
	return &executorClient{baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Transport: noKeepAliveTransport}}
}

func (c *executorClient) CreateSandbox(ctx context.Context, req execproto.CreateSandboxRequest) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("executor: marshal create sandbox: %w", err)
	}
	var out struct {
		SandboxID string `json:"sandbox_id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes", body, &out); err != nil {
		return "", err
	}
	return out.SandboxID, nil
}

// Execute 消费 SSE 流：log 帧聚合为 Output，exit 帧为结果（幂等键防重试双执行）。
func (c *executorClient) Execute(ctx context.Context, sandboxID, name, input, idempotencyKey string) (*ExecResult, error) {
	body, err := json.Marshal(execproto.ExecuteRequest{
		SandboxID: sandboxID, Name: name, Input: input, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("executor: marshal execute: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/execute", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("executor: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("executor: POST /execute: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		body := strings.TrimSpace(string(b))
		// 沙箱不存在 → 哨兵（dispatch 据此重建走快照恢复，评审 #7）
		if resp.StatusCode == http.StatusNotFound && strings.Contains(body, "sandbox not found") {
			return nil, execproto.ErrSandboxNotFound
		}
		if resp.StatusCode == http.StatusConflict && strings.Contains(body, "sandbox destroyed") {
			// 409 destroyed → 哨兵（执行面与文件面统一：沙箱已销毁 = 需重建）
			return nil, execproto.ErrSandboxNotFound
		}
		return nil, fmt.Errorf("executor: POST /execute status %d: %s", resp.StatusCode, body)
	}
	return parseExecFrames(resp.Body)
}

func parseExecFrames(r io.Reader) (*ExecResult, error) {
	res := &ExecResult{}
	sawExit := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var f struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f); err != nil {
			return nil, fmt.Errorf("executor: bad frame: %w", err)
		}
		switch f.Type {
		case "log":
			var p struct {
				Payload string `json:"payload"`
			}
			_ = json.Unmarshal(f.Payload, &p)
			res.Output += p.Payload + "\n"
		case "exit":
			sawExit = true
			var p struct {
				Exit      int    `json:"exit"`
				Output    string `json:"output"`
				OutputRef string `json:"output_ref"`
				Truncated bool   `json:"truncated"`
			}
			if err := json.Unmarshal(f.Payload, &p); err != nil {
				return nil, fmt.Errorf("executor: bad exit payload: %w", err)
			}
			res.Exit = p.Exit
			res.OutputRef = p.OutputRef
			res.Truncated = p.Truncated
			if p.Output != "" {
				res.Output = p.Output // 大输出外置场景的内联前缀（截断）
			}
		case "error":
			var p struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(f.Payload, &p)
			return nil, fmt.Errorf("executor: %s", p.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("executor: read stream: %w", err)
	}
	// 终止帧强制（评审 #2）：无 exit 帧的流（空流/裸 JSON/截断）不得视为成功——
	// 零值 exit=0 曾静默进 journal，恢复结果被污染
	if !sawExit {
		return nil, fmt.Errorf("executor: 流无 exit 终止帧（空流或协议不符）")
	}
	return res, nil
}

// AcquireLease 调 executor 租约端点（返回 generation）。
func (c *executorClient) AcquireLease(ctx context.Context, sandboxID, runID, ttl string) (int64, error) {
	body, _ := json.Marshal(map[string]string{"run_id": runID, "ttl": ttl})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/sandboxes/%s/lease", c.baseURL, sandboxID), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("executor: acquire lease: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("executor: acquire lease status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Generation int64 `json:"generation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("executor: decode lease: %w", err)
	}
	return out.Generation, nil
}

// Snapshot 调 executor 快照端点（返回 ref：镜像|卷tar）。
func (c *executorClient) Snapshot(ctx context.Context, sandboxID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/sandboxes/%s/snapshot", c.baseURL, sandboxID), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("executor: snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("executor: snapshot status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Ref string `json:"snapshot_ref"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("executor: decode snapshot: %w", err)
	}
	return out.Ref, nil
}

// ReleaseLease 释放（幂等：generation 不符/已释放 → 忽略）。
func (c *executorClient) ReleaseLease(ctx context.Context, sandboxID string, generation int64) error {
	body, _ := json.Marshal(map[string]any{"generation": generation})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/sandboxes/%s/lease", c.baseURL, sandboxID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("executor: release lease: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("executor: release lease status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil // 409（旧代次）幂等忽略——新持有者已接管
}

func (c *executorClient) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/files/%s%s", c.baseURL, sandboxID, path), nil)
	if err != nil {
		return "", fmt.Errorf("executor: build read file: %w", err)
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("executor: GET file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		body := strings.TrimSpace(string(b))
		if resp.StatusCode == http.StatusNotFound && strings.Contains(body, "sandbox not found") {
			return "", execproto.ErrSandboxNotFound
		}
		return "", fmt.Errorf("executor: GET file status %d: %s", resp.StatusCode, body)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("executor: read file body: %w", err)
	}
	return string(b), nil
}

func (c *executorClient) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/files/%s%s", c.baseURL, sandboxID, path), strings.NewReader(content))
	if err != nil {
		return fmt.Errorf("executor: build write file: %w", err)
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("executor: PUT file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// 404 哨兵 → ErrSandboxNotFound（评审 #7 恢复链；blob e2e 实证
		// 缺此映射会 500 循环旧容器）
		return execproto.ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("executor: PUT file status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// doJSON 通用 JSON 往返。
func (c *executorClient) doJSON(ctx context.Context, method, path string, body []byte, out any) error {
	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("executor: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("executor: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("executor: %s %s status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("executor: decode: %w", err)
		}
	}
	return nil
}
