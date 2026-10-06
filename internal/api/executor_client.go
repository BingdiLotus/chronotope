package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// HTTPExecutor 是 executor 协议的最小 HTTP 客户端（skill 安装的文件写入）。
type HTTPExecutor struct {
	Base string
	HTTP *http.Client
}

func NewHTTPExecutor(base string) *HTTPExecutor {
	return &HTTPExecutor{Base: base, HTTP: &http.Client{}}
}

// WriteFile 裸体写文件（PUT /files/{sandbox}/{path}——内容即文件字节）。
func (e *HTTPExecutor) WriteFile(ctx context.Context, path, content string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, e.Base+path, bytes.NewBufferString(content))
	if err != nil {
		return err
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("executor %s: %s", path, resp.Status)
	}
	return nil
}

func (e *HTTPExecutor) Call(ctx context.Context, path, method string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, e.Base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("executor %s: %s", path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
