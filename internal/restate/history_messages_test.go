package restate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

func appendHistoryRow(t *testing.T, st *fakeStore, run string, step int, role string, content any) {
	t.Helper()
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMessage(t.Context(), "session", run, step, role, raw); err != nil {
		t.Fatal(err)
	}
}

func historyAssistant(ids ...string) map[string]any {
	calls := make([]any, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, protocolToolCall(ToolCall{ID: id, Name: "remember", Arguments: json.RawMessage(`{"content":"old fact"}`)}))
	}
	return map[string]any{"content": "original assistant text", "tool_calls": calls}
}

func restoredHistory(t *testing.T, st *fakeStore) []runs.Message {
	t.Helper()
	messages, err := buildMessages(t.Context(), st, nil, RunInput{SessionID: "session", Input: "current request"}, "current", sessionapi.AgentConfig{ApplicationProfile: true, Instructions: "SOP"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertProviderToolGroups(t, messages)
	return messages
}

// Provider input requires every assistant tool ID exactly once before the next
// non-tool message, and never accepts an orphaned tool result.
func assertProviderToolGroups(t *testing.T, messages []runs.Message) {
	t.Helper()
	pending := map[string]bool{}
	for _, message := range messages {
		if message.Role == "tool" {
			if !pending[message.ToolCallID] {
				t.Fatalf("orphaned/duplicate tool result: %+v", message)
			}
			delete(pending, message.ToolCallID)
			continue
		}
		if len(pending) > 0 {
			t.Fatalf("missing tool results before %s: %+v", message.Role, pending)
		}
		if len(message.ToolCalls) > 0 && message.Role != "assistant" {
			t.Fatalf("tool calls on %s", message.Role)
		}
		for _, raw := range message.ToolCalls {
			var call struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &call); err != nil || call.ID == "" || pending[call.ID] {
				t.Fatalf("invalid assistant call: %s", raw)
			}
			pending[call.ID] = true
		}
	}
	if len(pending) > 0 {
		t.Fatalf("history ends without tool results: %+v", pending)
	}
}

func TestBuildMessagesDropsToolPrefixCutByRecentWindow(t *testing.T) {
	st := &fakeStore{}
	appendHistoryRow(t, st, "old", 0, "assistant", historyAssistant("a", "b"))
	appendHistoryRow(t, st, "old", 0, "tool", map[string]string{"tool_call_id": "a", "content": "old receipt A"})
	appendHistoryRow(t, st, "old", 0, "tool", map[string]string{"tool_call_id": "b", "content": "old receipt B"})
	// 51 stored rows: SQL's last 50 begin at the first tool result.
	for i := 0; i < 48; i++ {
		appendHistoryRow(t, st, "later", i, "user", fmt.Sprintf("later request %d", i))
	}
	messages := restoredHistory(t, st)
	for _, message := range messages {
		if message.Role == "tool" {
			t.Fatalf("window leaked an orphaned receipt: %+v", message)
		}
	}
	if len(messages) != 49 || messages[len(messages)-1].Content != "later request 47" {
		t.Fatalf("valid suffix lost: %+v", messages)
	}
	if len(st.messages) != 51 {
		t.Fatal("history repair modified stored records")
	}
}

func TestBuildMessagesCompletesUnconfirmedHistoricalCalls(t *testing.T) {
	for _, atEnd := range []bool{false, true} {
		t.Run(fmt.Sprintf("at-end-%v", atEnd), func(t *testing.T) {
			st := &fakeStore{}
			appendHistoryRow(t, st, "old", 0, "user", "old request")
			appendHistoryRow(t, st, "old", 1, "assistant", historyAssistant("a", "b", "c"))
			// A tool may have succeeded before its receipt write failed. Only B's
			// receipt is known; no execution status can be inferred for A or C.
			appendHistoryRow(t, st, "old", 1, "tool", map[string]string{"tool_call_id": "b", "content": "durable sink nonce", "source": "untrusted"})
			if !atEnd {
				appendHistoryRow(t, st, "current", 0, "user", "current request")
			}
			before := len(st.messages)
			messages := restoredHistory(t, st)
			var results []runs.Message
			for _, message := range messages {
				if message.Role == "tool" {
					results = append(results, message)
				}
				if len(message.ToolCalls) > 0 && message.Content != "original assistant text" {
					t.Fatal("stored assistant text lost")
				}
			}
			if len(results) != 3 || results[0].ToolCallID != "a" || results[1].ToolCallID != "b" || results[2].ToolCallID != "c" {
				t.Fatalf("call order/pairing changed: %+v", results)
			}
			if results[1].Content != "durable sink nonce" || results[1].Source != "untrusted" {
				t.Fatalf("known receipt changed: %+v", results[1])
			}
			for _, index := range []int{0, 2} {
				var unknown struct {
					ResultUnknown bool `json:"result_unknown"`
					Error         struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(results[index].Content), &unknown); err != nil || !unknown.ResultUnknown || unknown.Error.Code != "history_result_unconfirmed" || results[index].Source != "untrusted" {
					t.Fatalf("missing receipt was presented as confirmed: %+v", results[index])
				}
			}
			if len(st.messages) != before {
				t.Fatal("synthetic model view must not write provider receipts")
			}
		})
	}
}

func TestBuildMessagesRejectsForeignAndDuplicateToolResults(t *testing.T) {
	st := &fakeStore{}
	appendHistoryRow(t, st, "old", 3, "assistant", historyAssistant("a", "b"))
	appendHistoryRow(t, st, "different-run", 3, "tool", map[string]string{"tool_call_id": "b", "content": "foreign receipt"})
	appendHistoryRow(t, st, "old", 4, "tool", map[string]string{"tool_call_id": "b", "content": "foreign step receipt"})
	appendHistoryRow(t, st, "old", 3, "tool", map[string]string{"tool_call_id": "a", "content": "original receipt"})
	appendHistoryRow(t, st, "old", 3, "tool", map[string]string{"tool_call_id": "a", "content": "duplicate receipt"})
	appendHistoryRow(t, st, "current", 0, "user", "current request")
	appendHistoryRow(t, st, "old", 3, "tool", map[string]string{"tool_call_id": "b", "content": "orphan after new user"})
	messages := restoredHistory(t, st)
	encoded, _ := json.Marshal(messages)
	for _, forbidden := range []string{"foreign receipt", "foreign step receipt", "duplicate receipt", "orphan after new user"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("invalid result entered model history: %s", encoded)
		}
	}
	if !strings.Contains(string(encoded), "original receipt") || !strings.Contains(string(encoded), "history_result_unconfirmed") {
		t.Fatalf("known/unknown distinction lost: %s", encoded)
	}
}

func TestBuildMessagesDropsMalformedToolGroups(t *testing.T) {
	for _, ids := range [][]string{{"duplicate", "duplicate"}, {""}} {
		t.Run(fmt.Sprint(ids), func(t *testing.T) {
			st := &fakeStore{}
			appendHistoryRow(t, st, "old", 0, "tool", "unpaired legacy tool text")
			appendHistoryRow(t, st, "old", 1, "assistant", historyAssistant(ids...))
			appendHistoryRow(t, st, "old", 1, "tool", map[string]string{"tool_call_id": "duplicate", "content": "invalid group receipt"})
			appendHistoryRow(t, st, "current", 0, "user", "current request")
			messages := restoredHistory(t, st)
			for _, message := range messages {
				if message.Role == "tool" || len(message.ToolCalls) > 0 {
					t.Fatalf("malformed group survived: %+v", messages)
				}
			}
		})
	}
}
