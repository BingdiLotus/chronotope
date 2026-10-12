package policy

import (
	"context"
	"os"
	"testing"
)

// TestEnvResolverLayers 阶段 ①：env 后端的层优先级（裸名 > org 前缀）与
// 未配置的显式 nil。
func TestEnvResolverLayers(t *testing.T) {
	_ = os.Setenv("TEST_CRED_REF", "v-bare")
	_ = os.Setenv("o1_TEST_CRED_REF", "v-org")
	r := EnvResolver{}
	res, _ := r.Resolve(context.Background(), "", "TEST_CRED_REF")
	if res == nil || res.Value != "v-bare" || res.Source != "env" {
		t.Fatalf("裸名解析: %+v", res)
	}
	res, _ = r.Resolve(context.Background(), "o1", "TEST_CRED_REF")
	if res == nil || res.Value != "v-bare" {
		t.Fatalf("裸名优先于 org 前缀: %+v", res)
	}
	res, _ = r.Resolve(context.Background(), "", "MISSING_REF")
	if res != nil {
		t.Fatalf("未配置应显式 nil: %+v", res)
	}
	info := r.Describe(context.Background(), "", "TEST_CRED_REF")
	if !info.Configured || info.Source != "env" {
		t.Fatalf("describe: %+v", info)
	}
}
