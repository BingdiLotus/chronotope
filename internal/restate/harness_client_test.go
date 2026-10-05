package restate

import (
	"strings"
	"testing"
)

func frames(t *testing.T, lines ...string) *Result {
	t.Helper()
	res, err := ParseFrames(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}
	return res
}

func TestParseFramesDoneAggregatesDeltas(t *testing.T) {
	res := frames(t,
		`data: {"type":"beat","seq":0,"payload":{}}`,
		`data: {"type":"delta","seq":1,"payload":{"text":"你"}}`,
		`data: {"type":"delta","seq":2,"payload":{"text":"好"}}`,
		`data: {"type":"done","seq":3,"payload":{"final":"你好，请问有什么可以帮你？","usage":{"tokens_in":12,"tokens_out":8}}}`,
	)
	if !res.Done || res.ErrCode != "" {
		t.Fatalf("应识别 done 终帧: %+v", res)
	}
	if got := strings.Join(res.Deltas, ""); got != "你好" {
		t.Fatalf("delta 应按序聚合: %q", got)
	}
	if res.Final == "" || res.Usage.TokensIn != 12 || res.Usage.TokensOut != 8 {
		t.Fatalf("done payload 解析错误: %+v", res)
	}
}

func TestParseFramesErrorTerminal(t *testing.T) {
	res := frames(t,
		`data: {"type":"delta","seq":0,"payload":{"text":"部分"}}`,
		`data: {"type":"error","seq":1,"payload":{"code":"max_turns","message":"exceeded"}}`,
	)
	if res.Done || res.ErrCode != "max_turns" || res.ErrMsg != "exceeded" {
		t.Fatalf("error 是失败终态: %+v", res)
	}
}

func TestParseFramesTruncatedDone(t *testing.T) {
	res := frames(t,
		`data: {"type":"done","seq":0,"payload":{"final":"...","usage":{"tokens_in":1,"tokens_out":2,"usage_partial":true},"truncated":true}}`,
	)
	if !res.Done || !res.Truncated || !res.Usage.UsagePartial {
		t.Fatalf("截断语义字段应解析: %+v", res)
	}
}

func TestParseFramesToolCallVocabularyGuard(t *testing.T) {
	// 规范名：通过
	res := frames(t,
		`data: {"type":"tool_call","seq":0,"payload":{"id":"t_1","name":"bash","arguments":{"command":"pytest"}}}`,
		`data: {"type":"done","seq":1,"payload":{"final":"ok","usage":{"tokens_in":1,"tokens_out":1}}}`,
	)
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "bash" {
		t.Fatalf("tool_call 应解析: %+v", res.ToolCalls)
	}
	if string(res.ToolCalls[0].Arguments) != `{"command":"pytest"}` {
		t.Fatalf("arguments 应原样保留: %s", res.ToolCalls[0].Arguments)
	}

	// 非词汇表名：硬约束拒绝（框架自有工具名必须映射后发出）
	_, err := ParseFrames(strings.NewReader(
		`data: {"type":"tool_call","seq":0,"payload":{"id":"t_1","name":"run_shell","arguments":{}}}` + "\n",
	))
	if err == nil || !strings.Contains(err.Error(), "词汇表") {
		t.Fatalf("非规范名应被拒绝: %v", err)
	}
}

func TestParseFramesMissingTerminalFrame(t *testing.T) {
	_, err := ParseFrames(strings.NewReader(`data: {"type":"delta","seq":0,"payload":{"text":"x"}}` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "未见 done") {
		t.Fatalf("无终帧应报未完成错误: %v", err)
	}
}

func TestParseFramesBadJSON(t *testing.T) {
	if _, err := ParseFrames(strings.NewReader(`data: {oops` + "\n")); err == nil {
		t.Fatal("坏 JSON 帧应报错")
	}
}
