package session

import (
	"encoding/json"
	"testing"

	sessionapi "github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// TestRunRoundtrip 契约：Run 的不可变 command 序列化往返（期 7 下沉后
// core 是 kernel 的编译期契约——有验收）。
func TestRunRoundtrip(t *testing.T) {
	r := Run{ID: "r1", Input: "cmd", Topic: "default", SessionID: "s1", Status: sessionapi.RunQueued}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Run
	if err := json.Unmarshal(b, &back); err != nil || back.ID != "r1" || back.Input != "cmd" {
		t.Fatalf("roundtrip: %+v err=%v", back, err)
	}
}

// TestSessionDiffJSON 契约：分支差集的 JSON 形状（时间旅行 API 载荷）。
func TestSessionDiffJSON(t *testing.T) {
	d := SessionDiff{CommonPrefix: 3, OnlyA: []EventRow{{Seq: 4}}, OnlyB: []EventRow{{Seq: 5}}}
	b, _ := json.Marshal(d)
	var back SessionDiff
	if err := json.Unmarshal(b, &back); err != nil || back.CommonPrefix != 3 || len(back.OnlyA) != 1 || len(back.OnlyB) != 1 {
		t.Fatalf("diff json: %s %+v err=%v", b, back, err)
	}
}
