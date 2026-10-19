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
// 有限控制调用与异步工作流提交分开；HTTP 连接不承载运行生命周期。
type httpIngress struct {
	baseURL string
	client  *http.Client
}

// NewHTTPIngress 构造 ingress 客户端（restateURL 形如 http://restate:8080）。
func NewHTTPIngress(restateURL string) RestateIngress {
	return &httpIngress{
		baseURL: strings.TrimRight(restateURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Send waits only for Restate's durable acceptance, not workflow completion.
func (h *httpIngress) Send(ctx context.Context, path string, body any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return h.call(ctx, strings.TrimRight(path, "/")+"/send", http.MethodPost, body, nil, http.StatusAccepted)
}

// Call is reserved for short control operations such as session initialization.
func (h *httpIngress) Call(ctx context.Context, path, method string, body any, out any) error {
	return h.call(ctx, path, method, body, out, 0)
}

func (h *httpIngress) call(ctx context.Context, path, method string, body any, out any, expected int) error {
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || (expected != 0 && resp.StatusCode != expected) {
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
