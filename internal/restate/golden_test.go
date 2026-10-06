package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

// TestGoldenJournalReplay 契约规范 §7.5：golden journal 重放测试。
//
// 同一场景（4 步：write_file → bash → read_file → final）在全新 mock journal 上
// 独立执行两次（模拟崩溃后的重放）：两次轨迹必须完全一致（重放确定性）；
// 轨迹再与 checked-in 的 golden 文件逐行比对——任何记账/事件/状态语义漂移都会红。
//
// 更新 golden：UPDATE_GOLDEN=1 go test ./internal/restate/ -run TestGoldenJournalReplay
// （生成后需人工 review diff 再提交）。
func TestGoldenJournalReplay(t *testing.T) {
	scenario := func(t *testing.T) []string {
		st := &fakeStore{runs: map[string]*store.Run{"r_g": {ID: "r_g", SessionID: "s_g"}}}
		ha := &fakeHarness{script: []*Result{
			{Done: false, ToolCalls: []ToolCall{{ID: "t_1", Name: "write_file", Arguments: json.RawMessage(`{"path":"/workspace/a.py","content":"print(1)"}`)}}},
			{Done: false, ToolCalls: []ToolCall{{ID: "t_2", Name: "bash", Arguments: json.RawMessage(`{"command":"python3 /workspace/a.py"}`)}}},
			{Done: false, ToolCalls: []ToolCall{{ID: "t_3", Name: "read_file", Arguments: json.RawMessage(`{"path":"/workspace/a.py"}`)}}},
			{Done: true, Final: "沙箱闭环完成。"},
		}}
		se := &fakeSessions{state: SessionState{
			Phase: sessionapi.PhaseReady,
			AgentConfig: sessionapi.AgentConfig{
				Model: "claude-sonnet-4-6", Instructions: "你是助手。",
				Tools: []string{"write_file", "bash", "read_file"}, Version: 1,
			},
		}}
		ex := &fakeExecutor{files: map[string]string{"/workspace/a.py": "print(1)"}}

		out, err := runLoop(newMockedLoop(t), deps(st, ha, se, ex),
			RunInput{SessionID: "s_g", Input: "写代码并运行"}, "r_g")
		if err != nil {
			t.Fatalf("runLoop: %v", err)
		}
		if out.Final != "沙箱闭环完成。" || out.Steps != 4 {
			t.Fatalf("输出不符: %+v", out)
		}
		return traceOf(st, ha, ex)
	}

	first := scenario(t)
	second := scenario(t)

	// 1) 重放确定性：两次轨迹逐字节一致
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("重放轨迹不一致（记账非确定性）:\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s",
			strings.Join(first, "\n"), strings.Join(second, "\n"))
	}

	// 2) golden 比对
	got := strings.Join(first, "\n") + "\n"
	const golden = "testdata/golden/journal-replay.txt"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata/golden", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("写 golden: %v", err)
		}
		t.Logf("golden 已更新: %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("读 golden 文件失败（首次生成请 UPDATE_GOLDEN=1）: %v", err)
	}
	if got != string(want) {
		t.Fatalf("轨迹与 golden 不符（记账语义漂移）:\n--- got ---\n%s--- want ---\n%s", got, string(want))
	}
}

// traceOf 串行化一次执行的全部副作用（事件序 + 消息 + 终态 + harness 调用 + 沙箱操作序）。
func traceOf(st *fakeStore, ha *fakeHarness, ex *fakeExecutor) []string {
	var out []string
	for _, e := range st.events {
		out = append(out, fmt.Sprintf("event %s %s", e.typ, e.dedupe))
	}
	for _, m := range st.messages {
		out = append(out, "message "+m.role)
	}
	if r, ok := st.runs["r_g"]; ok {
		out = append(out, "status "+string(r.Status))
	}
	for _, c := range ha.calls {
		out = append(out, fmt.Sprintf("harness step=%d model=%s", c.Step, c.Model))
	}
	for _, op := range ex.ops {
		out = append(out, "sandbox "+op)
	}
	return out
}

var _ = context.Background // 保持导入稳定（fake 方法签名使用）
