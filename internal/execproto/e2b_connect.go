package execproto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 官方云 E2B 的命令执行走 envd 的 ConnectRPC（server-streaming JSON 帧）——
// 无 protoc 工具链依赖：手写 Connect 帧编解码（协议从 docs.e2b.dev 的
// openapi + envd process.proto 的 protobuf-ts 定义实证）。
//
// 帧格式（Connect unary/streaming JSON）：
//   flags(1B) + payloadLen(4B 大端) + UTF-8 JSON payload
//   请求 flags=0；响应终止帧 flags=2（payload 为 {} 或 {"error":{...}}）。

// ConnectEnv 是 connect-v2 的沙箱 envd 信息。
type ConnectEnv struct {
	SandboxID   string `json:"sandboxID"`
	AccessToken string `json:"envdAccessToken"`
	Domain      string `json:"domain"`
	Port        int    `json:"-"`
}

// connectV2 调平台 API 取 envd 访问令牌（若沙箱暂停会恢复；TTL 仅延长）。
func (c *HTTPE2BAPI) connectV2(ctx context.Context, sandboxID string) (*ConnectEnv, error) {
	body, _ := json.Marshal(map[string]any{"timeout": 300})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.Base+"/v2/sandboxes/"+sandboxID+"/connect", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: connect v2: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, fmt.Errorf("e2b: connect v2 status %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	var env ConnectEnv
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("e2b: connect v2 decode: %w", err)
	}
	env.Port = 49983 // envd 默认端口（openapi 实证）
	return &env, nil
}

// StartProcess 走 envd 的 process.Process/Start（Connect server-streaming）：
// 返回聚合 stdout/stderr 与退出码。
func (c *HTTPE2BAPI) StartProcess(ctx context.Context, sandboxID, cmd string) (string, string, int, error) {
	env, err := c.connectV2(ctx, sandboxID)
	if err != nil {
		return "", "", 0, err
	}
	host := "https://sandbox.e2b.app"
	if env.Domain != "" {
		if strings.HasPrefix(env.Domain, "http") {
			host = env.Domain // 自托管/测试场景（裸域名默认官方 https 宿主）
		} else {
			host = "https://" + env.Domain
		}
	}
	// 请求帧：单个 StartRequest 消息
	payload, _ := json.Marshal(map[string]any{
		"process": map[string]any{"cmd": "/bin/bash", "args": []string{"-c", cmd}},
		"stdin":   false,
	})
	frame := make([]byte, 5+len(payload))
	frame[0] = 0
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		host+"/process.Process/Start", bytes.NewReader(frame))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Timeout-Ms", "300000")
	req.Header.Set("E2b-Sandbox-Id", sandboxID)
	req.Header.Set("E2b-Sandbox-Port", fmt.Sprint(env.Port))
	req.Header.Set("X-Access-Token", env.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("e2b: process start: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return "", "", 0, fmt.Errorf("e2b: process start status %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}

	// 响应帧流：解析到终止帧（flags&2）
	var stdout, stderr []byte
	exit := -1
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		for len(buf) >= 5 {
			length := int(binary.BigEndian.Uint32(buf[1:5]))
			if len(buf) < 5+length {
				break
			}
			var msg struct {
				Event struct {
					Start struct {
						Pid int `json:"pid"`
					} `json:"start"`
					Data struct {
						Stdout string `json:"stdout"`
						Stderr string `json:"stderr"`
					} `json:"data"`
					End struct {
						ExitCode int    `json:"exit_code"`
						Status   string `json:"status"`
						Error    string `json:"error"`
					} `json:"end"`
				} `json:"event"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(buf[5:5+length], &msg); err == nil {
				if msg.Error != nil {
					return "", "", 0, fmt.Errorf("e2b: process error: %s: %s", msg.Error.Code, msg.Error.Message)
				}
				if msg.Event.End.Status != "" || msg.Event.End.ExitCode != 0 {
					if msg.Event.End.Error != "" {
						return "", "", 0, fmt.Errorf("e2b: process end: %s", msg.Event.End.Error)
					}
					exit = msg.Event.End.ExitCode
				}
				// bytes 字段的 Connect JSON 编码为 base64
				if d, err := base64.StdEncoding.DecodeString(msg.Event.Data.Stdout); err == nil {
					stdout = append(stdout, d...)
				} else if msg.Event.Data.Stdout != "" {
					stdout = append(stdout, msg.Event.Data.Stdout...)
				}
				if d, err := base64.StdEncoding.DecodeString(msg.Event.Data.Stderr); err == nil {
					stderr = append(stderr, d...)
				} else if msg.Event.Data.Stderr != "" {
					stderr = append(stderr, msg.Event.Data.Stderr...)
				}
			}
			terminal := buf[0]&2 != 0
			buf = buf[5+length:]
			if terminal {
				return string(stdout), string(stderr), exit, nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return string(stdout), string(stderr), exit, nil
			}
			return "", "", 0, fmt.Errorf("e2b: read stream: %w", rerr)
		}
	}
}
