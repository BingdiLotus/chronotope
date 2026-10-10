package policy

import (
	"context"
	"testing"

	sessionapi "github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

type fakeLookup struct {
	active map[string]string
}

func (f fakeLookup) ActiveHarness(_ context.Context, orgID, name string) (string, string, error) {
	if v, ok := f.active[name]; ok {
		return v, "v2", nil
	}
	return "", "", context.Canceled
}

// TestDefaultResolverChain 解析链：agent 绑定 → org active → 全局 fallback。
func TestDefaultResolverChain(t *testing.T) {
	d := DefaultResolver{
		Lookup:    fakeLookup{active: map[string]string{"claude": "http://h1:8000"}},
		GlobalURL: "http://global:8000",
	}
	// agent 绑定命中
	r, err := d.Resolve(context.Background(), "o1", &sessionapi.HarnessBinding{Name: "claude"})
	if err != nil || r.Endpoint != "http://h1:8000" || r.Version != "v2" {
		t.Fatalf("绑定命中: %+v err=%v", r, err)
	}
	// 绑定查不到 → 全局 fallback（fail-open 显式）
	r, _ = d.Resolve(context.Background(), "o1", &sessionapi.HarnessBinding{Name: "missing"})
	if r.Endpoint != "http://global:8000" || r.Version != "global" {
		t.Fatalf("绑定缺失应全局: %+v", r)
	}
	// 零绑定 → 全局
	r, _ = d.Resolve(context.Background(), "o1", nil)
	if r.Endpoint != "http://global:8000" {
		t.Fatalf("零绑定应全局: %+v", r)
	}
}
