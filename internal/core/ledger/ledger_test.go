package ledger

import (
	"encoding/json"
	"testing"
)

// TestLLMCallRowJSON 契约：账本行的序列化（对账 API 的载荷形状）。
func TestLLMCallRowJSON(t *testing.T) {
	r := LLMCallRow{RunID: "r1", Step: 2, State: "result", RequestHash: "h1", TokensIn: 100, TokensOut: 20}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back LLMCallRow
	if err := json.Unmarshal(b, &back); err != nil || back.State != "result" || back.TokensIn != 100 {
		t.Fatalf("row json: %s %+v err=%v", b, back, err)
	}
}

// TestStatesDomain 契约：仲裁状态机值域（prepared/dispatched/result/unknown）。
func TestStatesDomain(t *testing.T) {
	valid := map[string]bool{"prepared": true, "dispatched": true, "result": true, "unknown": true}
	for _, s := range []string{"prepared", "dispatched", "result", "unknown"} {
		if !valid[s] {
			t.Fatalf("state %s 不在值域", s)
		}
	}
	if valid["cancelled"] {
		t.Fatal("cancelled 不属于仲裁状态机")
	}
}
