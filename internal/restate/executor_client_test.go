package restate

import (
	"strings"
	"testing"
)

// TestParseExecFramesRequiresExitFrame 终止帧强制（评审 #2）：空流/裸 JSON 报错，
// 缓存命中 SSE 流（beat+exit）正确解析。
func TestParseExecFramesRequiresExitFrame(t *testing.T) {
	// 空流 → 错误（曾静默成功 exit=0）
	if _, err := parseExecFrames(strings.NewReader("")); err == nil {
		t.Fatal("空流应报错（无终止帧）")
	}
	// 裸 JSON（旧缓存命中形态）→ 错误
	if _, err := parseExecFrames(strings.NewReader(`{"exit":0,"output":"x"}`)); err == nil {
		t.Fatal("裸 JSON 应报错（客户端固定解析 SSE）")
	}
	// 仅 log 帧无 exit → 错误
	if _, err := parseExecFrames(strings.NewReader("data: {\"type\":\"log\",\"payload\":\"x\"}\n\n")); err == nil {
		t.Fatal("无 exit 帧应报错")
	}
	// 缓存命中 SSE 流（beat + exit）→ 结果正确
	res, err := parseExecFrames(strings.NewReader(
		"data: {\"type\":\"beat\",\"payload\":{\"seq\":0}}\n\ndata: {\"type\":\"exit\",\"payload\":{\"exit\":2,\"output\":\"缓存的输出\"}}\n\n"))
	if err != nil || res.Exit != 2 || res.Output != "缓存的输出" {
		t.Fatalf("缓存流应解析 exit=2: %+v err=%v", res, err)
	}
	// 正常执行流（log + exit 非零）→ 非零 exit 传递（不再被忽略）
	res, err = parseExecFrames(strings.NewReader(
		"data: {\"type\":\"log\",\"payload\":\"ls\"}\n\ndata: {\"type\":\"exit\",\"payload\":{\"exit\":1,\"output\":\"失败\"}}\n\n"))
	if err != nil || res.Exit != 1 {
		t.Fatalf("失败 exit 应传递: %+v err=%v", res, err)
	}
}
