package checkpoint

import (
	"encoding/json"
	"testing"
)

// TestCheckpointJSON 契约：时间坐标的载荷形状（checkpoint API 的投影）。
func TestCheckpointJSON(t *testing.T) {
	c := Checkpoint{ID: "c1", SessionID: "s1", Seq: 10, SnapshotRef: "ref-1", MaxMessageID: 42}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Checkpoint
	if err := json.Unmarshal(b, &back); err != nil || back.Seq != 10 || back.SnapshotRef != "ref-1" || back.MaxMessageID != 42 {
		t.Fatalf("checkpoint json: %s %+v err=%v", b, back, err)
	}
}
