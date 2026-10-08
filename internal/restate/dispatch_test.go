package restate

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/execproto"
)

// TestCodeToolInputEmptyArgs 空参防御（生产形态基准待办 2：真实模型偶发
// arguments={}——曾把 {} 当命令执行 127）。
func TestCodeToolInputEmptyArgs(t *testing.T) {
	if _, err := codeToolInput(ToolCall{Name: runs.ToolBash, Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("bash 空 command 应报错")
	}
	if _, err := codeToolInput(ToolCall{Name: runs.ToolBash, Arguments: json.RawMessage(`{"command":"  "}`)}); err == nil {
		t.Fatal("bash 空白 command 应报错")
	}
	if _, err := codeToolInput(ToolCall{Name: runs.ToolRunPython, Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("run_python 空 code 应报错")
	}
	if got, err := codeToolInput(ToolCall{Name: runs.ToolBash, Arguments: json.RawMessage(`{"command":"echo ok"}`)}); err != nil || got != "echo ok" {
		t.Fatalf("正常参数应通过: %q %v", got, err)
	}
}

// TestSandboxGoneClassification 审计 P1-9：404 哨兵只限真实沙箱缺失——
// in-flight/权限/落库失败不得触发重建重试。
func TestSandboxGoneClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"真实缺失哨兵", execproto.ErrSandboxNotFound, true},
		{"契约文本 not found", fmt.Errorf("execute: sandbox not found"), true},
		{"destroyed 文本", fmt.Errorf("write: sandbox destroyed"), true},
		{"in-flight 不误判", fmt.Errorf("execute: 同键执行进行中（in-flight）"), false},
		{"权限拒绝不误判", fmt.Errorf("execute: permission denied"), false},
		{"落库失败不误判", fmt.Errorf("exec done: store write failed"), false},
		{"空错误", nil, false},
	}
	for _, c := range cases {
		if got := sandboxGone(c.err); got != c.want {
			t.Errorf("%s: sandboxGone=%v 期望 %v", c.name, got, c.want)
		}
	}
}
