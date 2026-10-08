package restate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

// TestBuildMessagesInjectsMemory 边界语义 §7 组装函数：主题摘要 + 检索片段注入 system。
func TestBuildMessagesInjectsMemory(t *testing.T) {
	st := &fakeStore{
		summaries: []store.Summary{
			{SessionID: "s_1", Topic: "default", Version: 1, Summary: "用户在做登录模块。"},
		},
		memoryItems: []store.MemoryItem{
			{SessionID: "s_1", Topic: "default", Kind: "long_term", Content: "用户偏好：Go 语言", ContentHash: "h1"},
			{SessionID: "s_1", Topic: "other", Kind: "long_term", Content: "无关主题", ContentHash: "h2"},
		},
		messages: []struct {
			sessionID, runID, role, content string
			step                            int
		}{{"s_1", "r_0", "user", `"历史消息"`, 0}},
	}
	cfg := sessionapi.AgentConfig{Model: "m", Instructions: "你是助手。", Version: 1}

	msgs, err := buildMessages(t.Context(), st, nil, RunInput{SessionID: "s_1", Input: "继续", Topic: "default"}, "r_1", cfg, nil)
	if err != nil {
		t.Fatalf("buildMessages: %v", err)
	}
	var systems []string
	for _, m := range msgs {
		if m.Role == "system" {
			systems = append(systems, m.Content)
		}
	}
	joined := strings.Join(systems, "\n")
	if !strings.Contains(joined, "【主题摘要】用户在做登录模块。") {
		t.Fatalf("应注入主题摘要: %v", systems)
	}
	if !strings.Contains(joined, "用户偏好：Go 语言") {
		t.Fatalf("应注入 topic 作用域检索片段: %v", systems)
	}
	if strings.Contains(joined, "无关主题") {
		t.Fatalf("不应注入其他 topic 的条目: %v", systems)
	}
	// 顺序纪律：摘要/记忆在历史与用户输入之前（组装函数唯一入口）
	if msgs[0].Role != "system" || msgs[len(msgs)-1].Role != "user" {
		t.Fatalf("消息顺序不符: %+v", msgs)
	}
}

// TestConsolidateTriggers 消化触发：消息数达阈值 → 摘要 + 条目 + memory.consolidated。
func TestConsolidateTriggers(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	for i := 0; i < 41; i++ { // 阈值 40：41 条消息触发
		st.messages = append(st.messages, struct {
			sessionID, runID, role, content string
			step                            int
		}{"s_1", "r_0", "user", `"问题` + string(rune('a'+i%26)) + `"`, 0})
	}
	ha := &fakeHarness{script: []*Result{
		{Done: true, Final: "你好。"},          // 主循环 step0
		{Done: true, Final: "摘要：对话围绕问题讨论。"}, // 摘要调用
	}}
	se := &fakeSessions{state: SessionState{
		Phase:       sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1},
	}}
	ex := &fakeExecutor{}
	deps := deps(st, ha, se, ex)
	deps.ConsolidateThreshold = 40

	out, err := runLoop(newMockedLoop(t), deps, RunInput{SessionID: "s_1", Input: "hi"}, "r_1")
	if err != nil || out.Final != "你好。" {
		t.Fatalf("主 run 应正常完成: out=%+v err=%v", out, err)
	}
	// 摘要调用发生（run_id 带 #consolidation 后缀）+ 摘要落库
	if len(ha.calls) != 2 || !strings.HasSuffix(ha.calls[1].RunID, "#consolidation") {
		t.Fatalf("应有 1 次摘要调用: %+v", ha.calls)
	}
	if len(st.createdSummaries) != 1 || st.createdSummaries[0].Summary != "摘要：对话围绕问题讨论。" {
		t.Fatalf("摘要应落库: %+v", st.createdSummaries)
	}
	// 条目抽取（最近 40 条中的 user 消息，哈希去重）
	if len(st.createdItems) == 0 {
		t.Fatal("应抽取记忆条目")
	}
	if got := eventsOf(st, event.MemoryConsolidated); len(got) != 1 {
		t.Fatalf("应有 1 条 memory.consolidated，得 %d", len(got))
	}
}

// TestConsolidateBelowThreshold 未达阈值不触发（零副作用）。
func TestConsolidateBelowThreshold(t *testing.T) {
	st := &fakeStore{runs: map[string]*store.Run{"r_1": {ID: "r_1", SessionID: "s_1"}}}
	st.messages = append(st.messages, struct {
		sessionID, runID, role, content string
		step                            int
	}{"s_1", "r_0", "user", `"一个问题"`, 0})
	ha := &fakeHarness{script: []*Result{{Done: true, Final: "你好。"}}}
	se := &fakeSessions{state: SessionState{
		Phase:       sessionapi.PhaseReady,
		AgentConfig: sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1},
	}}
	ex := &fakeExecutor{}
	deps := deps(st, ha, se, ex)
	deps.ConsolidateThreshold = 40

	if _, err := runLoop(newMockedLoop(t), deps, RunInput{SessionID: "s_1", Input: "hi"}, "r_1"); err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if len(ha.calls) != 1 {
		t.Fatalf("不应有摘要调用: %d", len(ha.calls))
	}
	if len(st.createdSummaries) != 0 || len(st.createdItems) != 0 {
		t.Fatalf("不应有任何记忆写入")
	}
	if got := eventsOf(st, event.MemoryConsolidated); len(got) != 0 {
		t.Fatalf("不应有 memory.consolidated")
	}
}

var _ = json.RawMessage{} // 保持导入（fake 消息构造）

// TestBuildMessagesKnowledgeInjection 期 3 §D：共享知识挂载注入（检索失败静默降级）。
func TestBuildMessagesKnowledgeInjection(t *testing.T) {
	st := &fakeStore{} // GetSession 返回假行（org_test）
	har := &fakeHarness{embed: []float32{0.1}}
	st.knowledge = []store.KnowledgeItem{{ID: "k1", TenantID: "o_1", Content: "共享知识片段"}}
	cfg := sessionapi.AgentConfig{Model: "m", Instructions: "i", Tools: []string{}, Version: 1}
	msgs, err := buildMessages(t.Context(), st, har, RunInput{SessionID: "s_1", Input: "hi", Topic: "default"}, "r_1", cfg, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "【共享知识】") && strings.Contains(m.Content, "共享知识片段") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应注入共享知识: %+v", msgs)
	}
	// 嵌入失败 → 静默降级（不阻断 run）
	har2 := &fakeHarness{embedErr: fmt.Errorf("embed down")}
	msgs2, err := buildMessages(t.Context(), st, har2, RunInput{SessionID: "s_1", Input: "hi", Topic: "default"}, "r_1", cfg, nil)
	if err != nil || len(msgs2) == 0 {
		t.Fatalf("嵌入失败应降级: %v", err)
	}
}
