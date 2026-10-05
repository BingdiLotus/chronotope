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
	"time"

	"github.com/bingdilotus/chronotope/internal/core/runs"
)

// Harness 是 /runs 协议客户端的最小接口（worker-架构设计 §1 HC；
// 唯一协议：POST /runs → SSE，契约规范 §3）。
type Harness interface {
	Call(ctx context.Context, req *runs.Request) (*Result, error)
}

// Result 是一次 /runs 调用的聚合结果（SSE 帧流 → 结构）。
type Result struct {
	Done      bool          // done 终帧（唯一合法终态）
	Deltas    []string      // delta 帧文本（按序）
	Final     string        // done 帧 final
	Usage     runs.LLMUsage // done 帧 usage（截断/断流时允许 usage_partial）
	Truncated bool          // done 帧 truncated（截断是 journaled 事实）
	ToolCalls []ToolCall    // tool_call 帧（交棒）
	ErrCode   string        // error 帧 code（失败终态）
	ErrMsg    string        // error 帧 message
}

// ToolCall 是交棒的工具调用（name 必须是词汇表规范名，契约规范 §3）。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// harnessClient 是 HTTP+SSE 实现；无状态（每次调用短连接）。
type harnessClient struct {
	baseURL string
	client  *http.Client
}

// noKeepAliveTransport 禁用连接复用（harness/executor 共用；见 NewHarnessClient 注释）。
var noKeepAliveTransport = &http.Transport{
	DisableKeepAlives:   true,
	MaxIdleConnsPerHost: -1,
}

// NewHarnessClient 构造 harness 客户端（baseURL 形如 http://harness:8000）。
func NewHarnessClient(baseURL string) Harness {
	return &harnessClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		// 分钟级 SSE 长流：不设总超时，仅依赖调用方 ctx 与帧间隔语义（契约规范 §3）；
		// 禁用连接复用——harness 无状态可随时重建（容器 IP 会变），池化死连接
		// 导致瞬时「unexpected EOF」（demo 场景实测），每次调用新建连接 + 新鲜 DNS
		client: &http.Client{Transport: noKeepAliveTransport},
	}
}

// Call 执行一次 /runs（幂等由 journal 缓存承担，harness 不实现缓存）。
func (h *harnessClient) Call(ctx context.Context, req *runs.Request) (*Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("harness: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/runs", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("harness: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := h.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("harness: POST /runs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("harness: POST /runs status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return ParseFrames(resp.Body)
}

// ParseFrames 解析 SSE 帧流为 Result（导出以便契约测试直接喂样例帧流）。
// 契约语义：done 是唯一合法终态；error 是失败终态；两者皆无 = 未完成（调用方重发）。
func ParseFrames(r io.Reader) (*Result, error) {
	res := &Result{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 容忍大 delta 帧

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var f runs.Frame
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f); err != nil {
			return nil, fmt.Errorf("harness: bad frame %q: %w", line, err)
		}
		switch f.Type {
		case runs.FrameDelta:
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(f.Payload, &p); err != nil {
				return nil, fmt.Errorf("harness: bad delta payload: %w", err)
			}
			res.Deltas = append(res.Deltas, p.Text)
		case runs.FrameToolCall:
			var p struct {
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(f.Payload, &p); err != nil {
				return nil, fmt.Errorf("harness: bad tool_call payload: %w", err)
			}
			if !runs.IsVocabularyName(p.Name) {
				return nil, fmt.Errorf("harness: tool name %q 不是词汇表规范名（契约规范 §3 硬约束）", p.Name)
			}
			res.ToolCalls = append(res.ToolCalls, ToolCall{ID: p.ID, Name: p.Name, Arguments: p.Arguments})
		case runs.FrameDone:
			var p runs.DonePayload
			if err := json.Unmarshal(f.Payload, &p); err != nil {
				return nil, fmt.Errorf("harness: bad done payload: %w", err)
			}
			res.Done = true
			res.Final = p.Final
			res.Usage = p.Usage
			res.Truncated = p.Truncated != nil && *p.Truncated
		case runs.FrameError:
			var p runs.ErrorPayload
			if err := json.Unmarshal(f.Payload, &p); err != nil {
				return nil, fmt.Errorf("harness: bad error payload: %w", err)
			}
			res.ErrCode, res.ErrMsg = p.Code, p.Message
		case runs.FrameBeat, runs.FrameTurnEnd:
			// 心跳/分段帧：仅透传语义，不进入结果
		default:
			return nil, fmt.Errorf("harness: unknown frame type %q", f.Type)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("harness: read stream: %w", err)
	}
	// 契约 §3 三种返回：done（终答）/ tool_call（交棒）/ error（失败）。
	// 三者皆无 = 未完成（连接断开未见终态 → 调用方幂等重发）。
	if !res.Done && res.ErrCode == "" && len(res.ToolCalls) == 0 {
		return nil, fmt.Errorf("harness: stream ended without done/error/tool_call terminal frame（契约规范 §3：连接断开未见 done = 未完成）")
	}
	return res, nil
}

// 心跳语义占位：帧间隔超时（30s 无帧）判定在调用方结合 ctx 处理（W1 简化）。
const frameGapTimeout = 30 * time.Second
