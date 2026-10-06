package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpIngress 是 Restate ingress 的 HTTP 客户端实现（api → worker 控制面，
// worker-架构设计 §8：run 启动信号 / 事件回调等 LISTEN/NOTIFY 之外的控制面调用）。
// 调用保持短连接 + 长超时：W1 的 run_workflow 为同步对话闭环（分钟级）。
type httpIngress struct {
	baseURL string
	client  *http.Client
}

// NewHTTPIngress 构造 ingress 客户端（restateURL 形如 http://restate:8080）。
func NewHTTPIngress(restateURL string) RestateIngress {
	return &httpIngress{
		baseURL: strings.TrimRight(restateURL, "/"),
		client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

// Call 调用 ingress 路径（Void 输入端点传 body=nil，不携带 content-type）。
func (h *httpIngress) Call(ctx context.Context, path, method string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("ingress: marshal body: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, rd)
	if err != nil {
		return fmt.Errorf("ingress: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("ingress: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ingress: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		// 空响应体（工作流 Void 结果）视为成功——decode EOF 容忍
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("ingress: decode response: %w", err)
		}
	}
	return nil
}
