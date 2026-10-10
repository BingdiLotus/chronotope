package wait

import (
	"encoding/json"
	"testing"
)

// TestDecisionSerialization 契约：DECIDE 判定的 journaled 载荷（scheduler.decide
// 事件的 payload 形状——hint/reason 稳定）。
func TestDecisionSerialization(t *testing.T) {
	d := Decision{Hint: HintWait, Reason: "waiting: timer"}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		Hint   string `json:"hint"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(b, &back); err != nil || back.Hint != "wait" || back.Reason == "" {
		t.Fatalf("decision json: %s %+v err=%v", b, back, err)
	}
}

// TestKindsStable 契约：四 wake kind 的字符串值稳定（Holon 收敛语义——
// 期 6 的 session_waits.kind 列值）。
func TestKindsStable(t *testing.T) {
	want := map[Kind]string{Timer: "timer", TaskResult: "task_result", External: "external", OperatorInput: "operator_input"}
	for k, v := range want {
		if string(k) != v {
			t.Fatalf("kind %s: %s", k, v)
		}
	}
}
