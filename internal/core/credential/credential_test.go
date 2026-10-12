package credential

import "testing"

// TestRefValidate 引用名的 POSIX 形态（nominal brand 的合法性）。
func TestRefValidate(t *testing.T) {
	if !Ref("ANTHROPIC_API_KEY").Validate() || !Ref("org1_TOKEN").Validate() {
		t.Fatal("合法引用应通过")
	}
	if Ref("").Validate() || Ref("1BAD").Validate() || Ref("has-dash").Validate() || Ref("空间 名").Validate() {
		t.Fatal("非法引用应拒绝")
	}
}
