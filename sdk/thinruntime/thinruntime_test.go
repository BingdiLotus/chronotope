package thinruntime

import "testing"

// TestEffectKeyConformance M5：幂等键与平台同构（cachekey 语义——
// harness (run,step) / tool exec:step:toolID）。
func TestEffectKeyConformance(t *testing.T) {
	k := EffectKey{RunID: "r_1", Step: 3, Tool: "bash", ID: "t_9"}
	if k.HarnessKey() != "r_1:3" {
		t.Fatalf("harness key: %q", k.HarnessKey())
	}
	if k.ToolKey() != "exec:3:t_9" {
		t.Fatalf("tool key: %q", k.ToolKey())
	}
	if Digest("x") == Digest("y") {
		t.Fatal("不同输入的摘要不得相同")
	}
}

// TestDecideReplay Halt M5：对账判定——result 同 hash 回读、unknown 停住。
func TestDecideReplayHalt(t *testing.T) {
	if d := Decide(&CallRow{State: "result", RequestHash: "h1"}, "h1"); d != "replay" {
		t.Fatalf("result 同 hash 应 replay: %s", d)
	}
	if d := Decide(&CallRow{State: "result", RequestHash: "h1"}, "h2"); d != "conflict" {
		t.Fatalf("result 异 hash 应 conflict: %s", d)
	}
	if d := Decide(&CallRow{State: "unknown"}, "h1"); d != "halt" {
		t.Fatalf("unknown 应 halt: %s", d)
	}
	if d := Decide(&CallRow{State: "dispatched"}, "h1"); d != "halt" {
		t.Fatalf("dispatched 无接受证据应 halt: %s", d)
	}
}
