package harness

import "testing"

// TestCapabilitiesHas B1：Has 判定原语（路由/校验的判定）。
func TestCapabilitiesHas(t *testing.T) {
	c := Default
	if !c.Has("steering") || !c.Has("PreToolUse") || !c.Has("read_only") || !c.Has("exec") {
		t.Fatalf("默认声明应全能力: %+v", c)
	}
	if c.Has("nonexistent") {
		t.Fatal("不存在的能力应 false")
	}
	var empty Capabilities
	if empty.Has("steering") {
		t.Fatal("空声明应 false")
	}
}
