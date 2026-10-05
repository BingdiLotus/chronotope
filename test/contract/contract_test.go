// 契约测试（W1 起随契约落库，落地方案 §6）：
// 事件 schema 序列化、去重键、缓存键、/runs 协议样例、execute 幂等键、journal 大小策略。
// 样例文件 testdata/*.json 与 docs/contracts/ 中的示例保持同源，任何改动必须同步。
package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/execproto"
	"github.com/bingdilotus/chronotope/internal/restate"
)

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

// --- 事件 schema ---

func TestEventTypeUniverse(t *testing.T) {
	// 类型全集成员必须自校验通过（契约规范 §5 只增不改）。
	all := []event.Type{
		event.RunStarted, event.RunCompleted, event.RunFailed, event.RunCancelled,
		event.LLMCall, event.ToolCall, event.SandboxExec, event.MCPCall,
		event.StepJournaled, event.EventTruncated,
		event.RunPaused, event.RunResumed, event.RunAwaitingApproval, event.RunFrozen,
		event.RunUnfrozen, event.SessionWoken,
		event.SkillInstall, event.MCPConnected, event.MCPUpdated,
		event.SubagentSpawned, event.SubagentCompleted, event.GroupTurn,
		event.MemoryConsolidated,
		event.AuditToolDenied, event.BudgetExceeded,
	}
	for _, typ := range all {
		if !typ.Valid() {
			t.Errorf("type %q should be valid", typ)
		}
	}
	if event.Type("run.exploded").Valid() {
		t.Error("unknown type must not be valid")
	}
}

func TestEventSampleRoundTrip(t *testing.T) {
	b := testdata(t, "events.sample.json")
	var ev event.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}
	if ev.SessionID != "s_1" || ev.RunID != "r_1" || ev.Type != event.SandboxExec || ev.Seq != 42 {
		t.Fatalf("unexpected sample content: %+v", ev)
	}
	if !ev.Type.Valid() {
		t.Fatalf("sample type invalid: %q", ev.Type)
	}
	// 回序列化必须稳定（字段顺序无关，语义一致即可）。
	var ev2 event.Event
	if err := json.Unmarshal(mustMarshal(t, ev), &ev2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if ev2.Seq != ev.Seq || ev2.Type != ev.Type || ev2.SessionID != ev.SessionID {
		t.Fatalf("round trip mismatch: %+v vs %+v", ev, ev2)
	}
}

func TestEventDedupeKey(t *testing.T) {
	// dedupe_key = run_id:step:kind[:tool]（契约规范 §5）
	if got := event.DedupeKey("r_1", 12, "sandbox.exec", ""); got != "r_1:12:sandbox.exec" {
		t.Fatalf("got %q", got)
	}
	if got := event.DedupeKey("r_1", 12, "tool.call", "bash"); got != "r_1:12:tool.call:bash" {
		t.Fatalf("got %q", got)
	}
}

// --- /runs 协议 ---

func TestRunsRequestSample(t *testing.T) {
	b := testdata(t, "runs.request.json")
	var req runs.Request
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("sample request must be valid: %v", err)
	}
	if req.Protocol != runs.ProtocolVersion || req.RunID != "r_1" || req.SessionID != "s_1" || req.Step != 12 {
		t.Fatalf("unexpected sample content: %+v", req)
	}
	if req.MaxTurns != 8 || req.MaxOutputBytes != 524288 {
		t.Fatalf("defaults mismatch: %+v", req)
	}
}

func TestRunsFramesSample(t *testing.T) {
	b := testdata(t, "runs.frames.ndjson")
	var frames []runs.Frame
	for _, line := range splitLines(t, string(b)) {
		var f runs.Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("unmarshal frame %q: %v", line, err)
		}
		frames = append(frames, f)
	}
	if len(frames) != 3 {
		t.Fatalf("want 3 frames, got %d", len(frames))
	}
	if frames[0].Type != runs.FrameDelta || frames[1].Type != runs.FrameToolCall {
		t.Fatalf("unexpected frame order: %+v", frames)
	}
	// 唯一合法终态：done（截断语义字段在位）
	last := frames[2]
	if last.Type != runs.FrameDone {
		t.Fatalf("last frame must be done, got %s", last.Type)
	}
	var done runs.DonePayload
	if err := json.Unmarshal(last.Payload, &done); err != nil {
		t.Fatalf("unmarshal done payload: %v", err)
	}
	if done.Truncated == nil || !*done.Truncated {
		t.Fatal("sample done payload must carry truncated:true（截断是 journaled 事实）")
	}
}

// TestRunsRequestToolSchemaNeverNull 防回归：tools 的 schema 空时必须省略，
// 不得序列化为 null（harness 侧 pydantic 严格校验——契约测试捕获过的真实 bug）。
func TestRunsRequestToolSchemaNeverNull(t *testing.T) {
	req := runs.Request{
		Protocol: runs.ProtocolVersion,
		RunID:    "r_1", SessionID: "s_1", Model: "m",
		Tools: []runs.Tool{{Type: "function", Name: "bash"}},
	}
	raw := mustMarshal(t, req)
	var m struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.Tools) != 1 {
		t.Fatalf("tools 数量: %d", len(m.Tools))
	}
	if v, ok := m.Tools[0]["schema"]; ok && v == nil {
		t.Fatalf("schema 不得为 null: %s", raw)
	}
}

func TestToolVocabulary(t *testing.T) {
	for _, name := range []string{
		runs.ToolBash, runs.ToolRunPython, runs.ToolReadFile, runs.ToolWriteFile,
		runs.ToolListFiles, runs.ToolWebSearch, runs.ToolHTTPRequest,
		runs.ToolRequestApproval, runs.ToolSpawnSubagent, "mcp:github:search_repos",
	} {
		if !runs.IsVocabularyName(name) {
			t.Errorf("%q must be a vocabulary name", name)
		}
	}
	for _, name := range []string{"run_shell", "exec_command", "files.read"} {
		if runs.IsVocabularyName(name) {
			t.Errorf("%q must NOT be a vocabulary name（框架自有工具名必须映射后发出）", name)
		}
	}
}

// --- 缓存键（权威定义：journal 位置，契约规范 §1）---

func TestCacheKeyIsJournalPosition(t *testing.T) {
	key := restate.CacheKey("r_1", "harness:12")
	if key != "r_1/harness:12" {
		t.Fatalf("got %q", key)
	}
	// 版本绑定在 run（契约规范 §1）：缓存键不得包含 provider/model 版本。
	runID, stepName, ok := restate.SplitCacheKey(key)
	if !ok || runID != "r_1" || stepName != "harness:12" {
		t.Fatalf("split got %q %q %v", runID, stepName, ok)
	}
	// 换模型不换键（模型/协议版本由 run 绑定承载）。
	if restate.CacheKey("r_1", "harness:12") != key {
		t.Fatal("cache key must depend only on journal position")
	}
}

func TestStepNames(t *testing.T) {
	if got := restate.StepName("harness", 12, ""); got != "harness:12" {
		t.Fatalf("got %q", got)
	}
	if got := restate.StepName("exec", 12, "t_3"); got != "exec:12:t_3" {
		t.Fatalf("got %q", got)
	}
}

// --- journal 大小策略（契约规范 §7 第 5 条）---

func TestSummarizeJournalSizePolicy(t *testing.T) {
	small := restate.Summarize(make([]byte, 1024), "pg")
	if small.Ref != nil || len(small.Inline) != 1024 {
		t.Fatal("small payload must stay inline")
	}
	big := restate.Summarize(make([]byte, 100*1024), "minio")
	if big.Ref == nil || big.Ref.Size != 100*1024 || len(big.Inline) != restate.MaxInlineBytes {
		t.Fatalf("big payload must be summarized + ref: %+v", big)
	}
}

// --- execute 幂等键（契约规范 §4）---

func TestExecuteIdempotencyKey(t *testing.T) {
	if got := execproto.ExecuteIdempotencyKey("r_1", 12, "t_3"); got != "r_1:12:t_3" {
		t.Fatalf("got %q", got)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func splitLines(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if line := s[start:i]; line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if line := s[start:]; line != "" {
		out = append(out, line)
	}
	return out
}
