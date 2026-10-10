package memory

import (
	"encoding/json"
	"testing"
)

// TestSummaryRoundtrip 契约：摘要行序列化（派生数据的投影形状）。
func TestSummaryRoundtrip(t *testing.T) {
	s := Summary{SessionID: "s1", Topic: "登录模块", Version: 2, Summary: "内容摘要"}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Summary
	if err := json.Unmarshal(b, &back); err != nil || back.Topic != "登录模块" || back.Version != 2 {
		t.Fatalf("summary json: %s %+v err=%v", b, back, err)
	}
}

// TestMemoryItemFields 契约：长期记忆条目的来源引用（ContentHash/SourceRunID
// 的去重与溯源字段稳定）。
func TestMemoryItemFields(t *testing.T) {
	m := MemoryItem{SessionID: "s1", Topic: "t", Kind: "long_term", Content: "c", ContentHash: "h", SourceRunID: "r1", SourceStep: 3}
	b, _ := json.Marshal(m)
	var back MemoryItem
	if err := json.Unmarshal(b, &back); err != nil || back.ContentHash != "h" || back.SourceRunID != "r1" {
		t.Fatalf("item json: %s %+v err=%v", b, back, err)
	}
}
