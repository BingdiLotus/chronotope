package restate

import (
	"encoding/json"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/runs"
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
